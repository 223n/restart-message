# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 概要

Windowsの再起動を検知してDiscord Webhookに通知する、Go製の単一バイナリCLI（**Windows 専用**）。
詳細な機能・運用手順は [README.md](README.md) を参照。コメントは英語、ユーザー向け文字列・READMEは日本語。

## ビルド・テスト・Lint

このマシンのGoは `C:\Program Files\Go\bin` にあるが、**セッションの Bash/PowerShell ツールには PATH が反映されていないことがある**。Bashでは先頭に追加する:

```bash
export PATH="/c/Program Files/Go/bin:$PATH"
```

```bash
go test ./...                          # 全テスト（internal/ の5パッケージすべて。非Windowsではルートpkgが黙って除外される）
go test -run TestLatest ./internal/detect   # 単一テスト
go vet ./...                           # 静的検査
go build -trimpath -ldflags "-s -w" -o bin/restart-message.exe .   # 手動ビルド
pwsh -File scripts/build.ps1           # 推奨ビルド（bin\restart-message.exe を生成）
pwsh -File scripts/build.ps1 -Arch arm64   # ARM64版（bin\restart-message_arm64.exe）
GOOS=windows GOARCH=arm64 go build ./...   # arm64 のコンパイル検査（CIも実行）
```

非Windows環境では、`//go:build windows` の付いたファイル（ルートの main / `task_windows` / `service_windows`、および `winevent/winevent.go`）はコンパイルされない。
`detect` / `notify` / `config` / `state` と `winevent/record.go`（`Record` 型と XML デコード）はタグなしなので、どのOSでもテストできる。

**注意**: 非Windowsで `go test ./...` を実行すると exit 0 で成功したように見えるが、ルートパッケージは全ファイルがタグ付きのため出力にすら現れず、黙って検査対象から外れる。同じ理由で `GOOS=windows` を付けない `govulncheck` も Windows 専用コードを見逃す。全体を検査するなら `GOOS=windows go vet ./...` を併用すること。

## アーキテクチャ

### 2 つの独立した通知経路

ツールは補完関係にある2系統を持つ。どちらも検知ロジック（`detect`）と送信ロジック（`notify`）を共有する。

1. **起動時通知（安全網）** — タスクスケジューラの「起動時」トリガー → `run` コマンド。
   Systemイベントログを読み、**直近の起動**を分類して送信。`state.json` で多重通知を防止（1起動につき1通）。ネットワーク未確立に備え `SendWithRetry` でリトライ。
   登録は [task_windows.go](task_windows.go)（`schtasks` + UTF-16LEのXML定義、SYSTEM・最上位権限・1分遅延）。`schtasks` は System32 の絶対パスで起動し、タスク定義XMLの一時ファイル名は `os.CreateTemp` で毎回変える。

2. **シャットダウン前通知** — Windowsサービス `restart-message-svc` → SCMの **PRESHUTDOWN** 通知 → `sendShutdownNotice`。
   **進行中の**シャットダウンを直近のEvent 1074から分類。短いタイムアウト＋少数回リトライで、シャットダウンを長く止めない。
   実装は [service_windows.go](service_windows.go)（`golang.org/x/sys/windows/svc`）。手動確認用に `shutdown-notice` コマンドあり（送信可否は `shutdownNoticeGate` がサービスと共通で判断し、既定で `notify_shutdown_start` を尊重。`-force` で無視して送信）。

**起動時通知の時間予算は3ファイルにまたがる**: `main.go` の `collectAttempts` / `collectWait`（＝ `collectWorstCase`）と `bootSendAttempts` / `bootSendWait`、`notify` の `sendTimeout` / `maxRetryAfter`、`task_windows.go` の `executionTimeLimit`（PT10M・`AllowHardTerminate=true`）。どれかを増やすときは最悪所要時間を計算し直すこと（`TestBootRunFitsTheScheduledTaskLimit` が足すのは `collectWorstCase` + 送信の最悪値なので、イベントログの準備待ちを延ばす `collectAttempts` の引き上げも同じ予算を食う）。`notify.WorstCaseDuration` を軸に、`discord_test.go` の `TestSendWithRetry_WorstCaseFitsTheScheduledTaskLimit` と `main_test.go`（Windows専用）の `TestBootRunFitsTheScheduledTaskLimit` が守っている。

### パッケージの責務

- [main.go](main.go) — サブコマンドのディスパッチ、**表示整形**（Discord embedの構築 `buildMessage` / `buildPendingMessage`、カテゴリ→ラベル/色/絵文字の対応 `present`、`collect` によるブート検知のリトライ）、`run` の結果を `service.log` に書く `appLog` / `runLogLine`、登録前チェックの `installGate` / `checkInstallConfig`。判定ロジックそのものは持たない。
- [internal/winevent/](internal/winevent/) — `wevtapi.dll` をネイティブ呼び出しでイベントログを読む。`record.go`（`Record` 型・XMLデコード）はビルドタグなし、`winevent.go`（API呼び出し）は `//go:build windows`。この分割により `detect` はどのOSでもテストできる。DLLは `windows.NewLazySystemDLL` で System32 からのみ読み込む（SYSTEM権限で動くためDLLプリロード対策）。**`wevtutil`/PowerShell をシェルアウトしない**設計（依存削減＋コンソールのコードページ破損回避。UTF-16を明示デコード）。
- [internal/detect/](internal/detect/) — **本体の心臓部**。イベントログ群から最新ブートを分類する（`Latest`）／進行中シャットダウンを分類する（`PendingShutdown`）。
- [internal/notify/](internal/notify/) — Discord Webhook送信。`HTTPError.Permanent()` で永続エラー（**3xx すべて**と429以外の4xx）はリトライ打ち切り。**リダイレクトは追わない**（`CheckRedirect` が `ErrUseLastResponse` を返し、3xx をレスポンスとして扱う。POSTがGETに書き換わって本文が落ち、送っていないのに成功に見える事故を防ぐ）。リクエストにできない URL は `ErrInvalidRequest` で即中断（リトライ予算を空回りさせない）。Discordの要求に従い `User-Agent: DiscordBot (...)` を送る（`SetUserAgentVersion` で版を埋める）。**Webhook URL の秘密トークンをエラーに漏らさない**（`scrubURLError` が `*url.Error` を全層剥がし、`redactToken` が応答本文中のID/トークンを潰す）。プロキシは `http.DefaultTransport` 任せ（`HTTPS_PROXY` 等の環境変数のみ。Windowsのシステムプロキシは読まない）。
- [internal/config/](internal/config/) — 設定の読込。既定値 → 設定ファイル → 環境変数 `DISCORD_WEBHOOK_URL` の順で上書き。最終的なWebhook URLは `validateWebhookURL` で検証し、**https 以外は拒否**（ループバック宛ての http のみ許可）。返すパスは**実際に適用したファイル**だけを指す（読めなかった場合は空）。
- [internal/state/](internal/state/) — 通知済み状態の永続化（temp + renameのアトミック書込）。

### 検知ロジックの重要な不変条件（`detect` を編集する前に必読）

- **ロケール非依存**で判定する。表示言語に依存しないシグナル（1074の理由コード `param4`・開始プロセス `param1`、6008、Kernel-Power 41の `BugcheckCode`）を主に使う。文字列マッチは補助（`isPowerOff` の `param5` だけは数値の代替が無く、既知の文字列の集合）。
- **EventID だけでは同定しない**。1074（User32、比較は大文字小文字を無視）・6008/6005/6006（EventLog）・41（Kernel-Power）・12/13（Kernel-General）と、プロバイダーも必ず照合する。
- すべてのイベント対応付けは**現在のブートサイクル内**にスコープする（前回ブートマーカーと今回ブートの間）。古い1074を最新ブートに誤って紐付けないため。6008 と 41 は新しいブート側に記録されるため `nearBoot`（前後の窓＋「前回ブートより今回ブートに近い」条件）で拾い、前のサイクルの分が漏れ込まないようにする。
- **「前回ブート」も「1074より後のブート」も、時刻ではなくレコードの並び順（ログ順）で決める**。w32timeがブート直後に時計を進める／戻すため、タイムスタンプ比較では隣接サイクルを取り違える。`indexFirst` を通すのはこのため。`PendingShutdown` も同じ理由で、1074より後（＝より小さいindex）にブートマーカーがあれば「もう終わったシャットダウン」として false を返す。
- **分類の順序が重要**: 電源オフ判定（`isPowerOff`）はUpdateヒューリスティック（`isUpdate`）より**先**に評価する。計画済み・SYSTEM起因の完全シャットダウンを誤って `update` にしないため。
- Event 41（Kernel-Power）**単独では「予期しない」と判定しない**。通常再起動でも記録される（`BugcheckCode == 0`）。`crash` は `BugcheckCode != 0` のときのみ。`BugcheckCode` は文字列比較ではなく `parseHex` を通す（`"0x0"`・`"00"` を非ゼロと読まないため。解釈できなければ0＝crashではない、に倒す）。
- カテゴリは `update` / `manual` / `shutdown` / `unexpected` / `crash` / `unknown`。新カテゴリ追加時は `main.go` の `present` にラベル/色/絵文字も追加する。
- **`manual` は受け皿**であって「対話ユーザーが操作した」判定ではない。電源オフでも `update` でもない 1074 がすべてここに来る。`param7`（実行ユーザー）を読むのは `isUpdate` の3番目の規則（計画済 + `SYSTEM`。プロセス名を見ない）だけで、対話操作かどうかの判定には使っていない。README の「検知できる種別」表はこの実装をそのまま説明しているので、分類規則を変えたら表も直すこと。

### 設定・状態の探索パス

- 設定: `-config` → `RESTART_MESSAGE_CONFIG` → 実行ファイル隣の `config.json` → `%ProgramData%\restart-message\config.json`。Webhook URLは `DISCORD_WEBHOOK_URL` で上書き可。
- `-config` と `RESTART_MESSAGE_CONFIG` は**操作者の明示指定**なので、指すファイルが読めなければエラー。残る2つは探索パスなので不在は許容する（この非対称性は意図的）。
- `install` / `install-service` は、登録するタスクが実際に解決する設定を検証し、動かない構成なら**登録を拒否**する（`-allow-unconfigured` で上書き）。環境変数はSYSTEMから見えない可能性があるため判定から除外する。
- 状態・ログ: `%ProgramData%\restart-message\`（`state.json` / `service.log`）。SYSTEM/サービスからも読める場所を使う。`service.log` にはサービスだけでなく**起動時通知（`run`）の結果も1起動につき1行**書く（SYSTEMタスクにはコンソールが無く、以前は失敗が何も残らなかった）。Webhook URL は絶対に書かない。
- `config.json` は秘密情報を含むため `.gitignore` 済み。テンプレートは `config.example.json`。
- **ACLは一切設定していない**。Windowsでは `os.MkdirAll` にセキュリティ記述子が渡らず、`os.WriteFile` のパーミッションも読み取り専用属性の判定にしか使われないため、`0o755` / `0o644` を変えても出力は変わらない。`%ProgramData%\restart-message\` は `C:\ProgramData` のACL（既定で `BUILTIN\Users:(OI)(CI)(RX)`）を継承し、Webhookトークンを含む `config.json` は一般ユーザーが読める。README と SECURITY.md は `icacls` での手作業として案内している（継承解除と付与は**1回の `icacls` で**。分けると DACL が空の瞬間ができ、所有者がSYSTEMになっている再インストール時に2つ目が拒否される）。この手作業を行うと `%ProgramData%` 配下は管理者以外読めなくなり、`isConfigFile` が `fs.ErrNotExist` 以外の stat エラー（アクセス拒否）をエラーとして返すため、インストール先の実行ファイルでの `test` / `shutdown-notice` は非管理者では失敗する（`cmdStatus` だけは警告を出して検知結果の表示を続ける）。READMEはそれを前提に「管理者権限のPowerShellで」と書いている。Goのコードで絞る（`windows.SecurityAttributes` を渡す等）のは未実装で、やるなら両ドキュメントも同時に直す。

## ブランチ運用とリリース

- **git-flow (AVH Edition)**: `main`（安定版）/ `develop`（統合）/ `feature/*`・`release/*`・`hotfix/*`。PRの既定ベースは `develop`。
- リリースはGitHub Releaseをpublishすると [release.yml](.github/workflows/release.yml) が amd64/arm64 のバイナリ・`SHA256SUMS.txt`・`THIRD_PARTY_NOTICES.txt`・provenanceを自動添付。バージョンは `-ldflags "-X main.Version=<tag>"` で埋め込む（既定は `main.go` の `Version`）。
- `release.yml` は署名の前に2つのゲートを通す。**タグが `main` の履歴上にあること**（タグ未pushでの公開はGitHubが `develop` の先端にタグを作るため）と、**vet / test / govulncheck**。govulncheck だけは結果が「コミットの関数」ではなく「時刻の関数」なので、CI で通っていても再実行する。
- 週次の定期実行は既定ブランチ（`develop`）でしか起動しない。そのため `ci.yml` の govulncheck と `codeql.yml` は、定期実行時だけ `develop` と `main` の2本に分岐する。CodeQL は結果の帰属を `analyze` の `ref`/`sha` で明示する（既定だと `main` の解析結果が `develop` のアラートとして登録される）。
- **バージョン番号を書く場所は `main.go` の `Version` だけ**。`build.ps1` もREADMEの例もここから導出する（過去に両方で二重管理になり、リリースのたびに古くなっていた）。`THIRD_PARTY_NOTICES.txt` に依存ライブラリの版を書かないのも同じ理由。
- arm64 バイナリは x64 ランナー上で**実行できない**ため、`release.yml` の起動スモークテストは amd64 限定。arm64 は `go version -m` で `GOARCH` を検査するに留まる。
- CI: [ci.yml](.github/workflows/ci.yml)（Windowsランナーで gofmt / vet / build / test と govulncheck。windows/arm64 はクロスビルドのみで実行はしない）、[codeql.yml](.github/workflows/codeql.yml)（Windowsランナーでネイティブビルドして解析）、Dependabot（PRは `develop` 宛）。
