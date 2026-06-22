//go:build windows

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/223n/restart-message/internal/config"
	"github.com/223n/restart-message/internal/detect"
	"github.com/223n/restart-message/internal/notify"
	"github.com/223n/restart-message/internal/winevent"
)

const serviceName = "restart-message-svc"

// serviceHandler implements svc.Handler. It accepts PRESHUTDOWN so the SCM
// notifies it (with minutes of grace, network still up) before Windows actually
// shuts down or restarts.
type serviceHandler struct {
	cfgPath string
}

func (h *serviceHandler) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown | svc.AcceptPreShutdown
	changes <- svc.Status{State: svc.StartPending}
	changes <- svc.Status{State: svc.Running, Accepts: accepted}
	svcLog(h.cfgPath, "service running")

	for {
		req := <-r
		switch req.Cmd {
		case svc.Interrogate:
			changes <- req.CurrentStatus
		case svc.Stop:
			changes <- svc.Status{State: svc.StopPending}
			svcLog(h.cfgPath, "service stopped")
			return false, 0
		case svc.PreShutdown, svc.Shutdown:
			// Advertise a WaitHint above the bounded send budget (and bump
			// CheckPoint) so the SCM doesn't deem us hung while we block in
			// sendShutdownNotice.
			changes <- svc.Status{State: svc.StopPending, WaitHint: 25000, CheckPoint: 1}
			svcLog(h.cfgPath, fmt.Sprintf("shutdown signal received (cmd=%d)", req.Cmd))
			if err := sendShutdownNotice(h.cfgPath, false); err != nil {
				svcLog(h.cfgPath, "shutdown notice error: "+err.Error())
			}
			return false, 0
		default:
			svcLog(h.cfgPath, fmt.Sprintf("unexpected control request (cmd=%d)", req.Cmd))
		}
	}
}

// cmdService runs as the Windows service (invoked by the SCM).
func cmdService(args []string) error {
	fs := flag.NewFlagSet("service", flag.ExitOnError)
	cfgPath := fs.String("config", "", "設定ファイルのパス")
	fs.Parse(args)

	isSvc, err := svc.IsWindowsService()
	if err != nil {
		return err
	}
	if !isSvc {
		return fmt.Errorf("このコマンドは Windows サービスとして実行されます。手動テストは 'shutdown-notice' を使用してください")
	}
	return svc.Run(serviceName, &serviceHandler{cfgPath: *cfgPath})
}

// sendShutdownNotice posts the pre-shutdown notification. force bypasses the
// notify_shutdown_start config gate (used by the manual shutdown-notice test).
// It uses short timeouts so it never holds up shutdown for long; the boot-time
// notifier is the safety net if delivery fails.
func sendShutdownNotice(cfgPath string, force bool) error {
	cfg, _, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("設定の読み込みに失敗: %w", err)
	}
	if !force && !cfg.NotifyShutdownStart {
		svcLog(cfgPath, "notify_shutdown_start is disabled; skipping")
		return nil
	}
	if strings.TrimSpace(cfg.WebhookURL) == "" {
		return fmt.Errorf("discord_webhook_url が未設定です（config または DISCORD_WEBHOOK_URL）")
	}

	// Best-effort: classify the in-progress shutdown from the most recent 1074.
	// A small limit is plenty (records are newest-first), keeping shutdown latency low.
	var res *detect.Result
	if records, qerr := winevent.Query(detect.SystemChannel, detect.EventQuery, 50); qerr == nil {
		if r, ok := detect.PendingShutdown(records, time.Now().UTC(), 5*time.Minute); ok {
			res = r
		}
	} else {
		svcLog(cfgPath, "event query failed: "+qerr.Error())
	}

	msg := buildPendingMessage(cfg, res)

	// Two quick attempts with a short per-attempt timeout (bounded ~17s). Stop
	// early on a permanent error (bad webhook / invalid payload) — retrying it
	// cannot help.
	for i := 0; i < 2; i++ {
		err = notify.Send(cfg.WebhookURL, msg, 8*time.Second)
		if err == nil {
			svcLog(cfgPath, "shutdown notice sent")
			return nil
		}
		var he *notify.HTTPError
		if errors.As(err, &he) && he.Permanent() {
			break
		}
		if i == 0 {
			time.Sleep(1500 * time.Millisecond)
		}
	}
	return err
}

// cmdInstallService registers the service (auto-start, LocalSystem) and starts it.
func cmdInstallService(args []string) error {
	fs := flag.NewFlagSet("install-service", flag.ExitOnError)
	cfgPath := fs.String("config", "", "サービスが使用する設定ファイルのパス")
	fs.Parse(args)

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.Abs(exe)

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("サービスマネージャへの接続に失敗（管理者権限で実行してください）: %w", err)
	}
	defer m.Disconnect()

	if existing, oerr := m.OpenService(serviceName); oerr == nil {
		existing.Close()
		return fmt.Errorf("サービス %q は既に存在します。先に uninstall-service を実行してください", serviceName)
	}

	svcArgs := []string{"service"}
	if *cfgPath != "" {
		abs, _ := filepath.Abs(*cfgPath)
		svcArgs = append(svcArgs, "-config", abs)
	}

	s, err := m.CreateService(serviceName, exe, mgr.Config{
		DisplayName:  "Restart Message (shutdown notifier)",
		Description:  "シャットダウン/再起動の開始時に Discord へ通知します (restart-message)",
		StartType:    mgr.StartAutomatic,
		ServiceType:  windowsOwnProcess,
		ErrorControl: mgr.ErrorNormal,
	}, svcArgs...)
	if err != nil {
		return fmt.Errorf("サービスの作成に失敗: %w", err)
	}
	defer s.Close()

	// Explicit pre-shutdown grace, comfortably above our bounded send budget
	// (the OS default is 180s; we set it explicitly for clarity/robustness).
	if perr := setPreshutdownTimeout(s.Handle, 60_000); perr != nil {
		svcLog(*cfgPath, "set preshutdown timeout failed: "+perr.Error())
	}

	if err := s.Start(); err != nil {
		// Not fatal: it will start automatically on the next boot.
		fmt.Printf("サービスを登録しましたが、即時開始に失敗しました（次回起動時に開始されます）: %v\n", err)
	} else {
		fmt.Printf("サービス %q を登録し、開始しました（自動起動）。\n", serviceName)
	}
	fmt.Printf("  実行ファイル: %s\n  引数        : %s\n", exe, strings.Join(svcArgs, " "))
	return nil
}

// cmdUninstallService stops and removes the service.
func cmdUninstallService(args []string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("サービスマネージャへの接続に失敗（管理者権限で実行してください）: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("サービス %q が見つかりません: %w", serviceName, err)
	}
	defer s.Close()

	// Stop first and wait briefly for Stopped, so Delete doesn't race a running
	// process (which would leave the service "marked for deletion").
	if st, cerr := s.Control(svc.Stop); cerr != nil {
		// ERROR_SERVICE_NOT_ACTIVE and "cannot accept control" are benign here.
		svcLog("", "stop during uninstall: "+cerr.Error())
	} else {
		deadline := time.Now().Add(10 * time.Second)
		for st.State != svc.Stopped && time.Now().Before(deadline) {
			time.Sleep(300 * time.Millisecond)
			q, qerr := s.Query()
			if qerr != nil {
				break
			}
			st = q
		}
	}
	if err := s.Delete(); err != nil {
		return fmt.Errorf("サービスの削除に失敗: %w", err)
	}
	fmt.Printf("サービス %q を削除しました。\n", serviceName)
	return nil
}

// windowsOwnProcess is SERVICE_WIN32_OWN_PROCESS.
const windowsOwnProcess = 0x00000010

// servicePreshutdownInfo mirrors SERVICE_PRESHUTDOWN_INFO.
type servicePreshutdownInfo struct {
	PreshutdownTimeout uint32 // milliseconds
}

// setPreshutdownTimeout sets how long the SCM waits for this service during the
// pre-shutdown phase.
func setPreshutdownTimeout(h windows.Handle, ms uint32) error {
	info := servicePreshutdownInfo{PreshutdownTimeout: ms}
	return windows.ChangeServiceConfig2(h, windows.SERVICE_CONFIG_PRESHUTDOWN_INFO, (*byte)(unsafe.Pointer(&info)))
}

// svcLog appends a diagnostic line to %ProgramData%\restart-message\service.log.
func svcLog(cfgPath, msg string) {
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
