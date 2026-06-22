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

// Version is the build version (overridable via -ldflags "-X main.Version=...").
var Version = "0.1.0"

func main() {
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
  run        再起動を検知し、未通知なら Discord に通知する（タスクスケジューラ用）
  test       設定された Webhook にテスト通知を送る
  status     検知結果を表示するだけ（送信しない）
  install    起動時に run を実行するタスクを登録する（要管理者権限）
  uninstall  登録したタスクを削除する（要管理者権限）
  version    バージョンを表示
  help       このヘルプを表示

共通オプション:
  -config <path>   設定ファイルのパス
  -state  <path>   状態ファイルのパス
  -force           状態を無視して必ず通知する（run）
  -dry-run         送信せず内容のみ表示する（run）
`)
}

func defaultStatePath() string {
	return filepath.Join(config.ProgramDataDir(), "state.json")
}

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
			time.Sleep(5 * time.Second)
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
		return fmt.Errorf("設定の読み込みに失敗: %w", err)
	}
	sp := *statePath
	if sp == "" {
		sp = defaultStatePath()
	}
	st := state.Load(sp)

	_, res, err := collect(6)
	if err != nil {
		return err
	}

	if !*force && st.AlreadyNotified(res.RecordID, res.BootTime) {
		fmt.Printf("この起動(%s)は通知済みです。何もしません。\n", res.BootTime.Local().Format("2006-01-02 15:04:05"))
		return nil
	}

	notifyThis := cfg.ShouldNotify(string(res.Category))

	if *dryRun {
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
		return saveState(sp, res)
	}

	if strings.TrimSpace(cfg.WebhookURL) == "" {
		return fmt.Errorf("discord_webhook_url が未設定です（config または DISCORD_WEBHOOK_URL）")
	}
	if err := notify.SendWithRetry(cfg.WebhookURL, buildMessage(cfg, res), 12, 15*time.Second); err != nil {
		return fmt.Errorf("Discord 送信に失敗: %w", err)
	}
	fmt.Printf("通知を送信しました: %s (%s)\n", res.Category, res.BootTime.Local().Format("2006-01-02 15:04:05"))
	return saveState(sp, res)
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
	_, res, err := collect(1)
	if err != nil {
		return err
	}
	printResult(res, cfgUsed)
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
	if cfgPath != "" {
		fmt.Printf("  設定ファイル: %s\n", cfgPath)
	} else {
		fmt.Printf("  設定ファイル: (なし — 既定値を使用)\n")
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
