//go:build windows

package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

const taskName = "restart-message"

// executionTimeLimit is how long Task Scheduler lets one `run` last before
// AllowHardTerminate (below) kills it mid-flight. It has to stay above the worst
// case of what cmdRun does:
//
//	collect                : (6-1) × 5s sleeps                       =  25s
//	notify.SendWithRetry   : 12 × 20s per-attempt HTTP timeout        = 240s
//	                       + 11 × max(15s, notify.maxRetryAfter)
//
// where 6 and 5s are collect's own retry loop (main.go), 12 and 15s are cmdRun's
// arguments to notify.SendWithRetry, the 20s per-attempt timeout is hardcoded
// inside SendWithRetry, and notify.maxRetryAfter
// (internal/notify/discord.go) caps how far a server-supplied 429 Retry-After
// may stretch one of those sleeps.
//
// notify.maxRetryAfter is currently 20s, so the real worst case is
// 25 + 240 + 11×20 = 485s. A cap of 30s (25 + 240 + 11×30 = 595s) is the largest
// this arithmetic alone admits, but it is not the operative limit: internal/notify's
// TestSendWithRetry_WorstCaseFitsTheScheduledTaskLimit binds the same sum to a
// deliberately stricter ceiling — 600s less ~30s for collect less a 60s margin,
// i.e. 510s — because 595 of 600 leaves five seconds for six wevtapi queries that
// carry no timeout of their own, which is rounding error and not margin. Moving
// any of the four constants named above means redoing this arithmetic and reading
// that test, which is what fails when the redo is skipped.
//
// A cap of 60s does not fit at all: the sleeps alone are 11×60 = 660s and the
// send is 12×20 + 11×60 = 900s, over the limit before collect's 25s is even
// added — the task is then hard-terminated mid-send, having spent the whole
// boot's budget notifying nobody.
//
// Raising this limit instead would be the wrong half to move: a SYSTEM process
// sleeping for a quarter of an hour after every boot costs more than the last
// few attempts, which by then are failing anyway.
const executionTimeLimit = 10 * time.Minute

// isoMinutes renders a whole number of minutes as the ISO 8601 duration Task
// Scheduler's schema wants. executionTimeLimit is a time.Duration rather than
// the literal "PT10M" so that a test in this package can compare it against
// notify.WorstCaseDuration without an ISO 8601 parser; this converts it back at
// the one place the XML needs it. Whole minutes only, which is all the limit has
// ever been and all the schema needs here.
func isoMinutes(d time.Duration) string {
	return fmt.Sprintf("PT%dM", int(d/time.Minute))
}

// taskXMLTemplate is a Task Scheduler definition: run as SYSTEM at boot (with a
// short delay so the network is up), highest privileges, once per boot.
const taskXMLTemplate = `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>Windows 再起動を検知して Discord に通知する (restart-message)</Description>
    <URI>\restart-message</URI>
  </RegistrationInfo>
  <Triggers>
    <BootTrigger>
      <Enabled>true</Enabled>
      <Delay>PT1M</Delay>
    </BootTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>S-1-5-18</UserId>
      <RunLevel>HighestAvailable</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <ExecutionTimeLimit>%TIMELIMIT%</ExecutionTimeLimit>
    <Priority>7</Priority>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>%COMMAND%</Command>
      <Arguments>%ARGS%</Arguments>
    </Exec>
  </Actions>
</Task>
`

func cmdInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	cfgPath := fs.String("config", "", "登録するタスクが使用する設定ファイルのパス")
	allowUnconfigured := fs.Bool("allow-unconfigured", false, "設定を解決できなくてもタスクを登録する")
	fs.Parse(args)

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.Abs(exe)

	// Resolved before the check as well as before the XML: the absolute path is
	// what gets baked into the task arguments, so it is what the check must
	// resolve. A failure here is fatal rather than ignored — falling back to the
	// empty string would drop -config silently and register a task pointed
	// somewhere else entirely.
	absCfg := ""
	if *cfgPath != "" {
		absCfg, err = filepath.Abs(*cfgPath)
		if err != nil {
			return fmt.Errorf("-config のパスを絶対パスに変換できません: %w", err)
		}
	}
	// Refuse to register a task that cannot work. This is the last moment a human
	// is watching: from here the task runs as SYSTEM at boot, and a failure there
	// reaches only %ProgramData%\restart-message\service.log.
	if err := installGate(absCfg, false, *allowUnconfigured); err != nil {
		return err
	}

	taskArgs := "run"
	if absCfg != "" {
		taskArgs = fmt.Sprintf(`run -config "%s"`, absCfg)
	}

	xml := taskXMLTemplate
	// Order matters. The values substituted last are operator-controlled — an
	// executable path or a -config path — and a later ReplaceAll would rewrite a
	// placeholder that appeared inside one of them. Substituting the constant
	// first means the only text scanned for "%TIMELIMIT%" is the template's own.
	xml = strings.ReplaceAll(xml, "%TIMELIMIT%", isoMinutes(executionTimeLimit))
	xml = strings.ReplaceAll(xml, "%COMMAND%", xmlEscape(exe))
	xml = strings.ReplaceAll(xml, "%ARGS%", xmlEscape(taskArgs))

	// os.CreateTemp rather than a constant name under %TEMP%: `install` can run as
	// LocalSystem (an RMM or PsExec -s deployment, not the documented interactive
	// admin flow), and a fixed name is one an unprivileged user can create first.
	// os.WriteFile would truncate and reuse that file — handing the attacker the
	// contents of a task definition SYSTEM is about to register — while CreateTemp
	// opens a random name with O_EXCL, so a squatted name is refused, not adopted.
	tf, err := os.CreateTemp("", "restart-message-task-*.xml")
	if err != nil {
		return err
	}
	tmp := tf.Name()
	defer os.Remove(tmp)
	if _, err := tf.Write(toUTF16LEWithBOM(xml)); err != nil {
		tf.Close()
		return err
	}
	if err := tf.Close(); err != nil {
		return err
	}

	sch, viaFallback := schtasksPath()
	cmd := exec.Command(sch, "/Create", "/TN", taskName, "/XML", tmp, "/F")
	out, err := cmd.CombinedOutput()
	fmt.Print(decodeConsole(out))
	if err != nil {
		return schtasksFailure("/Create", sch, viaFallback, err)
	}
	fmt.Printf("タスク %q を登録しました。次回起動時から通知されます。\n", taskName)
	fmt.Printf("  実行ファイル: %s\n  引数        : %s\n", exe, taskArgs)
	return nil
}

func cmdUninstall(args []string) error {
	sch, viaFallback := schtasksPath()
	cmd := exec.Command(sch, "/Delete", "/TN", taskName, "/F")
	out, err := cmd.CombinedOutput()
	fmt.Print(decodeConsole(out))
	if err != nil {
		return schtasksFailure("/Delete", sch, viaFallback, err)
	}
	fmt.Printf("タスク %q を削除しました。\n", taskName)
	return nil
}

// schtasksPath returns the absolute path of schtasks.exe inside the real system
// directory.
//
// A bare "schtasks" is resolved through %PATH%. exec.LookPath refuses the
// current directory (ErrDot), but it still walks %PATH% in order and applies
// PATHEXT per directory, so a schtasks.com planted in any earlier user-writable
// entry beats System32\schtasks.exe — and both install and uninstall run
// elevated, so it would be run elevated. This is the same class of hole as the
// wevtapi.dll one closed in 0.4.1 with windows.NewLazySystemDLL, which pinned
// DLL loading to System32 and left executables alone. The precondition is
// uncommon: it needs a user-writable directory ahead of System32 in the
// machine's PATH.
//
// GetSystemDirectory rather than %SystemRoot%, because an attacker who can plant
// the binary can generally also set the environment, and the variable is exactly
// what would then be pointed elsewhere. GetWindowsDirectory is the fallback for
// the same reason. The literal is used only if both API calls fail, and it fails
// loudly (exec cannot find the file) on a Windows installed off C: rather than
// quietly running whatever %PATH% offers.
//
// viaFallback reports that the literal was used, i.e. that neither API answered
// and the path is a guess. It exists for the error message: failing closed is
// right, but "the file is not there" and "you are not an administrator" are
// different problems and used to print the same sentence.
func schtasksPath() (path string, viaFallback bool) {
	if dir, err := windows.GetSystemDirectory(); err == nil && dir != "" {
		return filepath.Join(dir, "schtasks.exe"), false
	}
	if dir, err := windows.GetWindowsDirectory(); err == nil && dir != "" {
		return filepath.Join(dir, "System32", "schtasks.exe"), false
	}
	return `C:\Windows\System32\schtasks.exe`, true
}

// schtasksFailure wraps a failed schtasks invocation in a message that matches
// the failure.
//
// Both call sites used to say 「管理者権限で実行してください」 unconditionally. That
// is the right guess for an exit status — schtasks itself reports access denied
// that way — but it is the wrong one for the path never having existed, which is
// what schtasksPath's literal fallback produces on a Windows installed off C:.
// exec reports that as ERROR_FILE_NOT_FOUND, which reaches here as fs.ErrNotExist;
// telling that operator to elevate a shell they already elevated is how a
// five-minute diagnosis becomes an afternoon.
//
// The resolved path is named in every case, because "which schtasks did it even
// try" is the first question either failure raises.
func schtasksFailure(op, path string, viaFallback bool, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		if viaFallback {
			return fmt.Errorf("schtasks が見つかりません（%s）。システムディレクトリを取得できなかったため既定のパスを使用しています。Windows の導入先を確認してください: %w", path, err)
		}
		return fmt.Errorf("schtasks が見つかりません（%s）。Windows の導入先を確認してください: %w", path, err)
	}
	return fmt.Errorf("schtasks %s に失敗しました（実行したのは %s。管理者権限で実行してください）: %w", op, path, err)
}

func xmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	)
	return r.Replace(s)
}

func toUTF16LEWithBOM(s string) []byte {
	u := utf16.Encode([]rune(s))
	buf := make([]byte, 0, len(u)*2+2)
	buf = append(buf, 0xFF, 0xFE) // UTF-16LE BOM
	for _, c := range u {
		buf = append(buf, byte(c), byte(c>>8))
	}
	return buf
}

// decodeConsole returns schtasks output as-is; schtasks writes OEM-encoded text
// but we only echo it for the user, so a best-effort string conversion is fine.
func decodeConsole(b []byte) string {
	return string(b)
}
