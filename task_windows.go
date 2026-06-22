//go:build windows

package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

const taskName = "restart-message"

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
    <ExecutionTimeLimit>PT10M</ExecutionTimeLimit>
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
	fs.Parse(args)

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.Abs(exe)

	taskArgs := "run"
	if *cfgPath != "" {
		abs, _ := filepath.Abs(*cfgPath)
		taskArgs = fmt.Sprintf(`run -config "%s"`, abs)
	}

	xml := taskXMLTemplate
	xml = strings.ReplaceAll(xml, "%COMMAND%", xmlEscape(exe))
	xml = strings.ReplaceAll(xml, "%ARGS%", xmlEscape(taskArgs))

	tmp := filepath.Join(os.TempDir(), "restart-message-task.xml")
	if err := os.WriteFile(tmp, toUTF16LEWithBOM(xml), 0o644); err != nil {
		return err
	}
	defer os.Remove(tmp)

	cmd := exec.Command("schtasks", "/Create", "/TN", taskName, "/XML", tmp, "/F")
	out, err := cmd.CombinedOutput()
	fmt.Print(decodeConsole(out))
	if err != nil {
		return fmt.Errorf("schtasks /Create に失敗しました（管理者権限で実行してください）: %w", err)
	}
	fmt.Printf("タスク %q を登録しました。次回起動時から通知されます。\n", taskName)
	fmt.Printf("  実行ファイル: %s\n  引数        : %s\n", exe, taskArgs)
	return nil
}

func cmdUninstall(args []string) error {
	cmd := exec.Command("schtasks", "/Delete", "/TN", taskName, "/F")
	out, err := cmd.CombinedOutput()
	fmt.Print(decodeConsole(out))
	if err != nil {
		return fmt.Errorf("schtasks /Delete に失敗しました（管理者権限で実行してください）: %w", err)
	}
	fmt.Printf("タスク %q を削除しました。\n", taskName)
	return nil
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
