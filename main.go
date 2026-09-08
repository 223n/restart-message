//go:build windows

// Command restart-message detects how/when Windows last restarted (via the
// System event log) and posts a notification to a Discord webhook. It is meant
// to run once per boot from a Task Scheduler "At startup" trigger.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/223n/restart-message/internal/config"
	"github.com/223n/restart-message/internal/detect"
	"github.com/223n/restart-message/internal/notify"
	"github.com/223n/restart-message/internal/state"
	"github.com/223n/restart-message/internal/winevent"
)

// configLoadFailed is passed to printResult in place of a path when config.Load
// returned an error. It cannot collide with a real path: Load returns either a
// path it actually read or the empty string.
const configLoadFailed = "\x00load-failed"

// Version is the build version (overridable via -ldflags "-X main.Version=...").
var Version = "0.6.0"

func main() {
	// The version lives only in this file, and notify cannot import it back
	// (main is the program). Hand it over once, before anything can send.
	notify.SetUserAgentVersion(Version)

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "run":
		err = cmdRun(args)
	case "test":
		err = cmdTest(args)
	case "status":
		err = cmdStatus(args)
	case "install":
		err = cmdInstall(args)
	case "uninstall":
		err = cmdUninstall(args)
	case "service":
		err = cmdService(args)
	case "install-service":
		err = cmdInstallService(args)
	case "uninstall-service":
		err = cmdUninstallService(args)
	case "shutdown-notice":
		err = cmdShutdownNotice(args)
	case "version", "-v", "--version":
		fmt.Printf("restart-message %s\n", Version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `restart-message — Windows 再起動を検知して Discord に通知します

使い方:
  restart-message <command> [options]

コマンド:
  run               再起動を検知し、未通知なら Discord に通知する（タスク用）
  test              設定された Webhook にテスト通知を送る
  status            検知結果を表示するだけ（送信しない）
  install           起動時に run を実行するタスクを登録する（要管理者権限）
  uninstall         登録したタスクを削除する（要管理者権限）
  service           Windows サービスとして実行する（SCM から起動。直接実行不可）
  install-service   シャットダウン前通知のサービスを登録する（要管理者権限）
  uninstall-service 上記サービスを削除する（要管理者権限）
  shutdown-notice   シャットダウン前通知を手動で1回送る（動作確認用。既定では
                    notify_shutdown_start を尊重。-force で無視して送信）
  version           バージョンを表示
  help              このヘルプを表示

共通オプション:
  -config <path>   設定ファイルのパス
  -state  <path>   状態ファイルのパス
  -force           状態を無視して必ず通知する（run）
                   notify_shutdown_start が false でも送信する（shutdown-notice）
  -dry-run         送信せず内容のみ表示する（run）
  -allow-unconfigured
                   設定を解決できなくても登録する（install / install-service）
`)
}

func defaultStatePath() string {
	return filepath.Join(config.ProgramDataDir(), "state.json")
}

// The boot-time run's own timing constants, named rather than written inline so
// that the test binding them to the scheduled task's execution limit reads the
// SAME values the code uses. Inline literals here would let the two drift apart
// silently, which is the whole failure the test exists to prevent.
const (
	// collectAttempts / collectWait: the event log may not carry every relevant
	// record the instant the task fires one minute after boot.
	collectAttempts = 6
	collectWait     = 5 * time.Second

	// bootSendAttempts / bootSendWait: the network is frequently not up yet.
	bootSendAttempts = 12
	bootSendWait     = 15 * time.Second
)

// collectWorstCase is how long collect can spend sleeping. The winevent queries
// themselves carry no timeout, so this is a floor on collect's cost, not a
// ceiling — the margin against executionTimeLimit has to absorb them.
const collectWorstCase = (collectAttempts - 1) * collectWait

// collect queries the System log, retrying up to attempts times because at boot
// the relevant events may not all be written the instant the task fires. Pass
// attempts=1 for an immediate, non-blocking probe (e.g. status).
func collect(attempts int) ([]winevent.Record, *detect.Result, error) {
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		records, err := winevent.Query(detect.SystemChannel, detect.EventQuery, 400)
		if err != nil {
			lastErr = err
		} else if res, ok := detect.Latest(records); ok {
			return records, res, nil
		}
		if attempt < attempts-1 {
			time.Sleep(collectWait)
		}
	}
	if lastErr != nil {
		return nil, nil, lastErr
	}
	return nil, nil, fmt.Errorf("起動イベント(Event 12)が見つかりませんでした")
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", "", "設定ファイルのパス")
	statePath := fs.String("state", "", "状態ファイルのパス")
	force := fs.Bool("force", false, "状態を無視して必ず通知する")
	dryRun := fs.Bool("dry-run", false, "送信せず内容のみ表示する")
	fs.Parse(args)

	cfg, cfgUsed, err := config.Load(*cfgPath)
	if err != nil {
		err = fmt.Errorf("設定の読み込みに失敗: %w", err)
		appLog(runLogLine(cfgUsed, nil, runConfigError, err))
		return err
	}
	sp := *statePath
	if sp == "" {
		sp = defaultStatePath()
	}
	st := state.Load(sp)

	_, res, err := collect(collectAttempts)
	if err != nil {
		appLog(runLogLine(cfgUsed, nil, runDetectError, err))
		return err
	}

	if !*force && st.AlreadyNotified(res.RecordID, res.BootTime) {
		fmt.Printf("この起動(%s)は通知済みです。何もしません。\n", res.BootTime.Local().Format("2006-01-02 15:04:05"))
		appLog(runLogLine(cfgUsed, res, runAlreadyNotified, nil))
		return nil
	}

	notifyThis := cfg.ShouldNotify(string(res.Category))

	if *dryRun {
		// Not logged: a dry run changes nothing, and it is only ever typed at a
		// console — the log exists for the run the scheduled task performs.
		fmt.Println("[dry-run] 検出結果（状態は変更しません）:")
		printResult(res, cfgUsed)
		if notifyThis {
			b, _ := json.MarshalIndent(buildMessage(cfg, res), "", "  ")
			fmt.Println("--- 送信ペイロード (Discord) ---")
			fmt.Println(string(b))
		} else {
			fmt.Printf("カテゴリ %q は通知対象外のため、本来は送信されません。\n", res.Category)
		}
		return nil
	}

	if !notifyThis {
		fmt.Printf("カテゴリ %q は通知対象外です。状態のみ更新します。\n", res.Category)
		serr := saveState(sp, res)
		appLog(runLogLine(cfgUsed, res, runNotInNotifyOn, serr))
		return serr
	}

	if strings.TrimSpace(cfg.WebhookURL) == "" {
		err := fmt.Errorf("discord_webhook_url が未設定です（config または DISCORD_WEBHOOK_URL）")
		appLog(runLogLine(cfgUsed, res, runNoWebhook, err))
		return err
	}
	// The worst case of this call plus collectWorstCase has to stay inside
	// executionTimeLimit (task_windows.go). TestBootRunFitsTheScheduledTaskLimit
	// binds the three of them together; it is the only thing that sees all three.
	if err := notify.SendWithRetry(cfg.WebhookURL, buildMessage(cfg, res), bootSendAttempts, bootSendWait); err != nil {
		err = fmt.Errorf("Discord 送信に失敗: %w", err)
		appLog(runLogLine(cfgUsed, res, runSendFailed, err))
		return err
	}
	fmt.Printf("通知を送信しました: %s (%s)\n", res.Category, res.BootTime.Local().Format("2006-01-02 15:04:05"))
	// A notification that was delivered but not recorded is the one failure that
	// repeats itself on the next boot, so it gets its own outcome rather than
	// hiding inside runSent with an error attached — the vocabulary is only
	// useful if the grep for "the send worked" does not also match it.
	serr := saveState(sp, res)
	outcome := runSent
	if serr != nil {
		outcome = runSentStateFailed
	}
	appLog(runLogLine(cfgUsed, res, outcome, serr))
	return serr
}

// Outcomes of one boot-time run, as they appear in the log. They are the whole
// vocabulary of that line, so an operator asking "why did I get no message" can
// tell a deliberate skip from a failure without reading this file.
const (
	runSent            = "sent"
	runSentStateFailed = "sent-state-write-failed" // delivered, but the next boot will notify again
	runAlreadyNotified = "skipped-already-notified"
	runNotInNotifyOn   = "skipped-not-in-notify_on"
	runConfigError     = "config-error"
	runDetectError     = "detect-error"
	runNoWebhook       = "no-webhook"
	runSendFailed      = "send-failed"
)

// runLogLine renders the one line cmdRun appends to the log per boot.
//
// It is a pure function of its arguments, which is what makes the format
// testable (main_test.go) and what makes "can the webhook URL end up in the log"
// answerable by reading this function alone: the URL is not a parameter, and
// must not become one.
//
// err is the only free-form field, and its sources were checked for the same
// reason — config.Load (whose messages deliberately never quote the URL),
// winevent.Query, state.Save (paths) and notify (which strips *url.Error down to
// its cause and redacts the token out of response bodies). It is also the only
// field with no length of its own, which is what maxLoggedErrorRunes answers.
//
// cfgUsed is config.Load's second return value: the file whose contents were
// actually applied. Empty is not a missing detail but the diagnosis of the most
// common misinstall — config.json left beside the build output while the
// registered task runs the copy in Program Files — so it is recorded explicitly
// rather than omitted.
func runLogLine(cfgUsed string, res *detect.Result, outcome string, err error) string {
	used := cfgUsed
	if used == "" {
		used = "(none)"
	}
	line := "run: outcome=" + outcome + " config=" + used
	if res != nil {
		line += fmt.Sprintf(" category=%s boot=%s", res.Category, res.BootTime.UTC().Format(time.RFC3339))
	}
	if err != nil {
		line += " error=" + truncateForLog(err.Error(), maxLoggedErrorRunes)
	}
	// One run must stay one line for the log to be greppable, and a Discord error
	// body reaches here verbatim (minus its token) and can be multi-line. Folding
	// after the truncation above is safe in both directions: it only ever replaces
	// one rune with one rune or two with one, so the line cannot grow past the
	// bound the truncation just imposed.
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(line)
}

// maxLoggedErrorRunes bounds the error text one log line may carry.
//
// It exists because notify.HTTPError keeps up to 4096 bytes of response body
// (io.LimitReader in notify.Send) and renders all of it from Error(), so a
// Discord outage page or a TLS-terminating proxy's error HTML would otherwise
// append ~4.2KB per boot to a file with no rotation and no size cap — and it
// would do so precisely in the scenario this log exists to record, a webhook
// that stays broken while nobody is looking at the machine. Triage needs the
// status code and the first line of the body; the rest is the part that grows.
const maxLoggedErrorRunes = 300

// truncateForLog cuts s to at most max runes, marking that it did.
//
// Runes, not bytes: the errors reaching here are largely Japanese, and slicing
// mid-sequence would put a replacement character in the log for no gain. The
// marker matters as much as the bound — a silently cut error reads as a
// different error.
func truncateForLog(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…(truncated)"
}

// appLog appends a diagnostic line to %ProgramData%\restart-message\service.log.
//
// It was svcLog until the boot-time path started using it too: that path runs as
// SYSTEM from Task Scheduler with no console and no task history by default, so
// until now every one of its failures — a rotated webhook answering 404 being
// the predictable one — went to os.Stderr and from there nowhere at all.
//
// The file keeps its service.log name: README documents that path, and a name
// this code cannot change in the same commit is worth less than the continuity.
func appLog(msg string) {
	dir := config.ProgramDataDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "service.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), msg)
}

func saveState(path string, res *detect.Result) error {
	return state.Save(path, &state.State{
		LastBootTime: res.BootTime,
		LastRecordID: res.RecordID,
		LastNotified: time.Now().UTC(),
	})
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	cfgPath := fs.String("config", "", "設定ファイルのパス")
	fs.Parse(args)

	_, cfgUsed, cerr := config.Load(*cfgPath)
	if cerr != nil {
		fmt.Fprintf(os.Stderr, "warning: 設定の読み込みに失敗しました: %v\n", cerr)
	}
	// Load returns an empty path on failure — nothing was applied, so naming a file
	// would claim it is in use. But printing the empty case's 「(なし — 既定値を使用)」
	// here would contradict the warning just above: the defaults are NOT in use
	// either, because Load returned no config at all.
	cfgState := cfgUsed
	if cerr != nil {
		cfgState = configLoadFailed
	}
	_, res, err := collect(1)
	if err != nil {
		return err
	}
	printResult(res, cfgState)
	return nil
}

func cmdTest(args []string) error {
	fs := flag.NewFlagSet("test", flag.ExitOnError)
	cfgPath := fs.String("config", "", "設定ファイルのパス")
	fs.Parse(args)

	cfg, _, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.WebhookURL) == "" {
		return fmt.Errorf("discord_webhook_url が未設定です（config または DISCORD_WEBHOOK_URL）")
	}
	host, _ := os.Hostname()
	msg := &notify.Message{
		Username:  cfg.Username,
		AvatarURL: cfg.AvatarURL,
		Embeds: []notify.Embed{{
			Title:     "✅ テスト通知",
			Color:     0x2ECC71,
			Fields:    []notify.EmbedField{{Name: "マシン", Value: nz(host), Inline: true}, {Name: "状態", Value: "restart-message は正常に設定されています", Inline: false}},
			Footer:    &notify.EmbedFooter{Text: "restart-message " + Version},
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		}},
	}
	if cfg.Mention != "" {
		msg.Content = cfg.Mention
	}
	if err := notify.Send(cfg.WebhookURL, msg, 20*time.Second); err != nil {
		return fmt.Errorf("Discord 送信に失敗: %w", err)
	}
	fmt.Println("テスト通知を送信しました。")
	return nil
}

// cmdShutdownNotice manually fires the pre-shutdown notification once, for
// verifying the service path without an actual shutdown.
//
// It honours notify_shutdown_start by default. README presents this command as
// 事前の動作確認, and until now it passed force=true: with notify_shutdown_start
// false the check sent a notice while the service it is meant to verify would
// stay silent, which is the opposite of what a pre-flight check is for. -force
// keeps the unconditional behaviour available for "send one anyway".
func cmdShutdownNotice(args []string) error {
	fs := flag.NewFlagSet("shutdown-notice", flag.ExitOnError)
	cfgPath := fs.String("config", "", "設定ファイルのパス")
	force := fs.Bool("force", false, "notify_shutdown_start が false でも送信する")
	fs.Parse(args)

	sent, skipped, err := sendShutdownNotice(*cfgPath, *force)
	if err != nil {
		return fmt.Errorf("シャットダウン通知の送信に失敗: %w", err)
	}
	if !sent {
		// Not an error: the configuration says not to send, and the service will
		// do the same thing at the next shutdown. Worded so it cannot be misread
		// as a successful send.
		fmt.Printf("送信しませんでした: %s\n", skipped)
		fmt.Println("実際のシャットダウン時も、サービスは同じ理由で送信しません。")
		fmt.Println("設定に関係なく1回だけ送るには -force を指定してください。")
		return nil
	}
	fmt.Println("シャットダウン前通知を送信しました。")
	return nil
}

// shutdownNoticeGate decides whether a pre-shutdown notice is sent. It is the
// single place that answers the question for both callers — the service at
// PRESHUTDOWN and the manual shutdown-notice command — so the manual check
// cannot disagree with the thing it is checking.
//
// The returned reason is user-facing text, because the only caller that skips
// with a console attached prints it verbatim.
func shutdownNoticeGate(notifyShutdownStart, force bool) (send bool, skipReason string) {
	if notifyShutdownStart || force {
		return true, ""
	}
	return false, "notify_shutdown_start が false です（config.json）"
}

// Environment variables this tool reads. internal/config owns the behaviour;
// the names are repeated here because the install gate has to name them to the
// operator, and a message that misspells the variable is worse than none.
const (
	envConfigVar  = "RESTART_MESSAGE_CONFIG"
	envWebhookVar = "DISCORD_WEBHOOK_URL"
)

// isPlaceholderWebhook reports whether the URL is still the sample shipped in
// config.example.json. That file's value is a valid https URL with a host and a
// webhook-shaped path, so every other check in this gate passes it — and the
// registered SYSTEM task then 404s on every boot, forever.
//
// It matches the sample's runs of X and Y rather than the whole string, so an
// operator who edited only the id or only the token is still caught. It is
// deliberately NOT a general "does this look like a Discord webhook" test: a
// user may legitimately point this at a proxy or a compatible endpoint, and a
// shape check would refuse those for no reason.
func isPlaceholderWebhook(raw string) bool {
	u := strings.TrimSpace(raw)
	return strings.Contains(u, "XXXXXXXX") || strings.Contains(u, "YYYYYYYY")
}

// installConfigFacts is what an install command observed about the
// configuration that the SYSTEM task (or service) it is about to register would
// resolve for itself.
//
// Every field is gathered by the caller so that the decision below stays a pure
// function — and so that "was a webhook URL resolved" can travel as a bool. The
// URL itself is never put in this struct: nothing downstream of the check would
// have a legitimate use for it, and both the console and the log are places it
// must not reach.
type installConfigFacts struct {
	Used           string // config.Load's second return: the file whose contents were applied
	LoadErr        error  // config.Load's error, if any
	WebhookSet     bool   // an effective webhook URL was resolved, environment excluded
	EnvConfigSet   bool   // RESTART_MESSAGE_CONFIG is set in the installing process
	EnvWebhookSet  bool   // DISCORD_WEBHOOK_URL is set in the installing process
	ForService     bool   // install-service rather than install
	ShutdownStart  bool   // cfg.NotifyShutdownStart; only read when LoadErr is nil
	ProgramDataCfg string // %ProgramData%\restart-message\config.json, quoted in the remedy

	// PlaceholderWebhook: the URL is still config.example.json's sample. README
	// tells the reader to copy that file, so the likeliest way to arrive at a
	// syntactically perfect webhook URL that 404s on every boot forever is to
	// forget to edit one line of it. A resolved URL is not the same as a
	// configured one, and the gate exists to catch exactly this class.
	PlaceholderWebhook bool
}

// installVerdict is the answer: whether registration may proceed, why not, and
// what to tell the operator either way.
type installVerdict struct {
	OK     bool
	Reason string   // why the registered task could not work; empty when OK
	Notes  []string // environment traps and remedies, printed under Reason
}

// checkInstallConfig decides whether install may register a task that will
// actually work.
//
// The failure it exists for: README says to put config.json in bin\, verify with
// `test`, copy the exe to Program Files and run `install` there. That registers
// a SYSTEM task which resolves no config at all, fails the missing-webhook check
// on every boot forever, and — with task history off by default — says so to
// nobody. The check is the only moment a human is watching.
//
// Environment variables are deliberately not allowed to satisfy it. A user
// RESTART_MESSAGE_CONFIG or DISCORD_WEBHOOK_URL is invisible to the SYSTEM
// identity the task runs as, and os.Getenv cannot tell a user variable from a
// machine-wide one, so counting either would turn the check into the thing it is
// meant to prevent: a pass that production does not honour. Refusing costs a
// machine-wide variable a false negative, which -allow-unconfigured answers in
// one flag; the other direction costs a silent boot notifier nobody finds.
//
// That same limit binds what the notes are allowed to claim, which is why they
// are worded as carefully as the decision. A machine-wide DISCORD_WEBHOOK_URL
// really is inherited by the SYSTEM task and the LocalSystem service, and README
// documents the variable as a supported way to supply the URL — so telling that
// operator their working configuration "fails on every boot" would be false, and
// would point them at the one remedy (対処3) whose wording is false for them too.
// The notes therefore state the exclusion as fact and both consequences as
// conditionals, and a refusal that coincides with a set variable grows a fourth
// remedy naming -allow-unconfigured for exactly that case.
//
// The notes also differ by verdict. On a pass there is no remedy list under them,
// so a note pointing at one would send the operator to text that is never
// printed; what is useful there is the file SYSTEM will actually read.
func checkInstallConfig(f installConfigFacts) installVerdict {
	target := "タスク"
	if f.ForService {
		target = "サービス"
	}
	v := installVerdict{OK: true}

	switch {
	case f.LoadErr != nil:
		v.OK = false
		v.Reason = fmt.Sprintf("登録する%sが使用する設定を読み込めません: %v", target, f.LoadErr)
	case !f.WebhookSet && f.Used == "":
		v.OK = false
		v.Reason = fmt.Sprintf("SYSTEM として実行される%sからは、設定ファイルが1つも見つかりません（discord_webhook_url を解決できません）", target)
	case !f.WebhookSet:
		v.OK = false
		v.Reason = fmt.Sprintf("設定ファイル %s に discord_webhook_url がありません", f.Used)
	case f.PlaceholderWebhook:
		v.OK = false
		v.Reason = fmt.Sprintf("設定ファイル %s の discord_webhook_url が config.example.json のままです", f.Used)
	}

	// The environment notes are printed whether or not the check passed: when it
	// failed they are usually the reason the operator believed it would pass, and
	// when it passed they still name a variable this check did not honour.
	if f.EnvWebhookSet {
		note := fmt.Sprintf("注意: 環境変数 %s が設定されていますが、ユーザー環境変数かシステム環境変数かをこの確認では判別できないため、除外して判定しました。", envWebhookVar)
		if v.OK {
			note += fmt.Sprintf("discord_webhook_url は %s から解決できているため、この登録はこの環境変数に依存しません。", appliedFileOrGeneric(f.Used))
		} else {
			note += "ユーザー環境変数であれば SYSTEM からは見えず、毎回の起動で失敗します。システム環境変数として設定済みであれば SYSTEM も参照できるため、下の対処4で登録してください。"
		}
		v.Notes = append(v.Notes, note)
	}
	if f.EnvConfigSet {
		note := fmt.Sprintf("注意: 環境変数 %s が設定されていますが、ユーザー環境変数かシステム環境変数かをこの確認では判別できないため、除外して判定しました。", envConfigVar)
		if v.OK {
			note += fmt.Sprintf("この登録が実際に読むのは %s です。", appliedFileOrGeneric(f.Used))
		} else {
			note += "ユーザー環境変数であれば SYSTEM からは見えないため、設定ファイルは -config か下の対処1の場所で指定してください。"
		}
		v.Notes = append(v.Notes, note)
	}
	if f.LoadErr == nil && f.ForService && !f.ShutdownStart {
		v.Notes = append(v.Notes, "注意: notify_shutdown_start が false のため、このサービスはシャットダウン前通知を送信しません（config.json で true にしてください）。")
	}
	if !v.OK {
		v.Notes = append(v.Notes,
			fmt.Sprintf("対処1: config.json を %s に置く（SYSTEM から読めます）", f.ProgramDataCfg),
			"対処2: -config <絶対パス> を指定する（登録時に引数へ埋め込まれます）",
			"対処3: 設定は後で行う場合のみ -allow-unconfigured を指定する（設定するまで通知は失敗し続けます）",
		)
		// Only when the variable is actually set, and only on a refusal: this is
		// the one remedy that does not end in "and then it works", so it must not
		// be offered to an operator who has no such variable to be right about.
		if f.EnvWebhookSet {
			v.Notes = append(v.Notes, fmt.Sprintf("対処4: %s をシステム環境変数（マシン全体）として設定済みなら SYSTEM も参照できるため、-allow-unconfigured を指定して登録する（ユーザー環境変数の場合は対処1か2が必要です）", envWebhookVar))
		}
	}
	return v
}

// appliedFileOrGeneric names the config file the check actually applied, for the
// passing notes. used is empty only in a case the real gatherInstallFacts cannot
// produce — it clears the environment before loading, so a resolved webhook URL
// came from a file — but checkInstallConfig is a pure function of its argument
// and must not print 「」 for a caller that hands it one anyway.
func appliedFileOrGeneric(used string) string {
	if used == "" {
		return "設定ファイル"
	}
	return used
}

// gatherInstallFacts resolves the configuration the way the registered task
// will, not the way this process happens to.
//
// The two environment variables are removed for the duration of the load, so
// what is measured is what a SYSTEM process with no user environment would find:
// -config as it will be baked into the task arguments, then config.json beside
// this executable (the same path the task will run), then %ProgramData%. Safe to
// do in place — install is single-goroutine and restores both immediately.
//
// Two things it still cannot see. It reads as the elevated administrator, not as
// SYSTEM, so a config.json that denies SYSTEM traverse or read passes here and
// fails at boot; and a machine-wide RESTART_MESSAGE_CONFIG or
// DISCORD_WEBHOOK_URL, which SYSTEM would in fact inherit, is treated as absent
// (see checkInstallConfig for why that direction is the safe one).
func gatherInstallFacts(explicitCfg string, forService bool) installConfigFacts {
	f := installConfigFacts{
		ForService:     forService,
		EnvConfigSet:   strings.TrimSpace(os.Getenv(envConfigVar)) != "",
		EnvWebhookSet:  strings.TrimSpace(os.Getenv(envWebhookVar)) != "",
		ProgramDataCfg: filepath.Join(config.ProgramDataDir(), "config.json"),
	}
	restore := clearEnv(envConfigVar, envWebhookVar)
	defer restore()

	cfg, used, err := config.Load(explicitCfg)
	f.Used, f.LoadErr = used, err
	if err == nil {
		f.WebhookSet = strings.TrimSpace(cfg.WebhookURL) != ""
		f.ShutdownStart = cfg.NotifyShutdownStart
		f.PlaceholderWebhook = isPlaceholderWebhook(cfg.WebhookURL)
	}
	return f
}

// clearEnv unsets the named variables and returns a function restoring exactly
// what was there — including the difference between "set to empty" and "unset",
// which config.Load's callers elsewhere in this process would otherwise see
// changed.
func clearEnv(names ...string) func() {
	saved := make(map[string]string, len(names))
	for _, n := range names {
		if v, ok := os.LookupEnv(n); ok {
			saved[n] = v
		}
		os.Unsetenv(n)
	}
	return func() {
		for n, v := range saved {
			os.Setenv(n, v)
		}
	}
}

// installGate is the pre-registration check shared by install and
// install-service. It prints what it found and returns an error when
// registration must be refused.
func installGate(explicitCfg string, forService, allowUnconfigured bool) error {
	v := checkInstallConfig(gatherInstallFacts(explicitCfg, forService))
	if v.Reason != "" {
		fmt.Fprintf(os.Stderr, "設定の確認に失敗しました: %s\n", v.Reason)
	}
	for _, n := range v.Notes {
		fmt.Fprintln(os.Stderr, n)
	}
	if v.OK {
		return nil
	}
	if !allowUnconfigured {
		return fmt.Errorf("動作しない登録になるため中止しました（意図的に設定なしで登録するには -allow-unconfigured を指定してください）")
	}
	fmt.Println("-allow-unconfigured が指定されているため、このまま登録します。")
	return nil
}

func printResult(res *detect.Result, cfgPath string) {
	label, _, emoji := present(res.Category)
	fmt.Printf("検出結果:\n")
	fmt.Printf("  種別        : %s %s (%s)\n", emoji, label, res.Category)
	fmt.Printf("  マシン      : %s\n", nz(res.Computer))
	fmt.Printf("  起動時刻    : %s\n", res.BootTime.Local().Format("2006-01-02 15:04:05 MST"))
	if res.ReasonText != "" {
		fmt.Printf("  理由        : %s\n", res.ReasonText)
	}
	if res.ReasonCode != 0 {
		fmt.Printf("  理由コード  : 0x%08X\n", res.ReasonCode)
	}
	if res.Process != "" {
		fmt.Printf("  プロセス    : %s\n", res.Process)
	}
	if res.User != "" {
		fmt.Printf("  ユーザー    : %s\n", res.User)
	}
	if res.ShutdownType != "" {
		fmt.Printf("  種類        : %s\n", res.ShutdownType)
	}
	if !res.PrevShutdown.IsZero() {
		fmt.Printf("  前回停止    : %s\n", res.PrevShutdown.Local().Format("2006-01-02 15:04:05"))
	}
	if res.Downtime > 0 {
		fmt.Printf("  ダウンタイム: %s\n", fmtDuration(res.Downtime))
	}
	if res.Detail != "" {
		fmt.Printf("  詳細        : %s\n", res.Detail)
	}
	switch cfgPath {
	case configLoadFailed:
		fmt.Printf("  設定ファイル: (読み込み失敗 — 上の warning を参照)\n")
	case "":
		fmt.Printf("  設定ファイル: (なし — 既定値を使用)\n")
	default:
		fmt.Printf("  設定ファイル: %s\n", cfgPath)
	}
}

// buildMessage renders a Result into a Discord embed.
func buildMessage(cfg *config.Config, res *detect.Result) *notify.Message {
	label, color, emoji := present(res.Category)
	fields := []notify.EmbedField{
		{Name: "マシン", Value: nz(res.Computer), Inline: true},
		{Name: "種別", Value: emoji + " " + label, Inline: true},
		{Name: "起動時刻", Value: res.BootTime.Local().Format("2006-01-02 15:04:05 MST"), Inline: false},
	}
	if res.ReasonText != "" {
		fields = append(fields, notify.EmbedField{Name: "理由", Value: res.ReasonText})
	}
	if res.Process != "" {
		fields = append(fields, notify.EmbedField{Name: "開始プロセス", Value: res.Process})
	}
	if res.User != "" {
		fields = append(fields, notify.EmbedField{Name: "実行ユーザー", Value: res.User, Inline: true})
	}
	if res.ShutdownType != "" {
		fields = append(fields, notify.EmbedField{Name: "種類", Value: res.ShutdownType, Inline: true})
	}
	if cfg.IncludeDowntime && res.Downtime > 0 {
		fields = append(fields, notify.EmbedField{Name: "ダウンタイム", Value: fmtDuration(res.Downtime), Inline: true})
	}
	if res.ReasonCode != 0 {
		fields = append(fields, notify.EmbedField{Name: "理由コード", Value: fmt.Sprintf("0x%08X", res.ReasonCode), Inline: true})
	}
	if res.Detail != "" {
		fields = append(fields, notify.EmbedField{Name: "詳細", Value: res.Detail})
	}

	msg := &notify.Message{
		Username:  cfg.Username,
		AvatarURL: cfg.AvatarURL,
		Embeds: []notify.Embed{{
			Title:     emoji + " PC が再起動しました",
			Color:     color,
			Fields:    fields,
			Footer:    &notify.EmbedFooter{Text: "restart-message " + Version},
			Timestamp: res.BootTime.UTC().Format(time.RFC3339),
		}},
	}
	if cfg.Mention != "" {
		msg.Content = cfg.Mention
	}
	return msg
}

// buildPendingMessage renders the pre-shutdown notice. res may be nil when the
// in-progress shutdown could not be classified (then a generic notice is sent).
func buildPendingMessage(cfg *config.Config, res *detect.Result) *notify.Message {
	host, _ := os.Hostname()
	computer := host
	color := 0xF1C40F // amber
	fields := []notify.EmbedField{}

	if res != nil {
		if res.Computer != "" {
			computer = res.Computer
		}
		label, c, emoji := present(res.Category)
		color = c
		fields = append(fields,
			notify.EmbedField{Name: "マシン", Value: nz(computer), Inline: true},
			notify.EmbedField{Name: "種別", Value: emoji + " " + label, Inline: true},
		)
		if res.ReasonText != "" {
			fields = append(fields, notify.EmbedField{Name: "理由", Value: res.ReasonText})
		}
		if res.Process != "" {
			fields = append(fields, notify.EmbedField{Name: "開始プロセス", Value: res.Process})
		}
		if res.User != "" {
			fields = append(fields, notify.EmbedField{Name: "実行ユーザー", Value: res.User, Inline: true})
		}
		if res.ShutdownType != "" {
			fields = append(fields, notify.EmbedField{Name: "種類", Value: res.ShutdownType, Inline: true})
		}
	} else {
		fields = append(fields,
			notify.EmbedField{Name: "マシン", Value: nz(computer), Inline: true},
			notify.EmbedField{Name: "状態", Value: "シャットダウン処理を開始しました"},
		)
	}

	msg := &notify.Message{
		Username:  cfg.Username,
		AvatarURL: cfg.AvatarURL,
		Embeds: []notify.Embed{{
			Title:     "⏻ まもなくシャットダウン/再起動します",
			Color:     color,
			Fields:    fields,
			Footer:    &notify.EmbedFooter{Text: "restart-message " + Version},
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		}},
	}
	if cfg.Mention != "" {
		msg.Content = cfg.Mention
	}
	return msg
}

// present maps a category to a Japanese label, embed color, and emoji.
func present(c detect.Category) (label string, color int, emoji string) {
	switch c {
	case detect.CatUpdate:
		return "Windows Update", 0x3498DB, "🔄"
	case detect.CatManual:
		return "手動の再起動", 0x2ECC71, "🙂"
	case detect.CatShutdown:
		return "シャットダウン→起動", 0x95A5A6, "⏻"
	case detect.CatUnexpected:
		return "予期しないシャットダウン", 0xE74C3C, "⚠️"
	case detect.CatCrash:
		return "ストップエラー(BSOD)", 0xE67E22, "💥"
	default:
		return "不明", 0x95A5A6, "❔"
	}
}

func fmtDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%d時間%d分", h, m)
	case m > 0:
		return fmt.Sprintf("%d分%d秒", m, s)
	default:
		return fmt.Sprintf("%d秒", s)
	}
}

func nz(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(不明)"
	}
	return s
}
