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
go test ./...                          # 全テスト（テストは detect / notify パッケージのみ）
go test -run TestLatest ./internal/detect   # 単一テスト
go vet ./...                           # 静的検査
go build -trimpath -ldflags "-s -w" -o bin/restart-message.exe .   # 手動ビルド
pwsh -File scripts/build.ps1           # 推奨ビルド（bin\restart-message.exe を生成）
```

非Windows環境では、ビルドタグ付きパッケージ（main / `winevent` / `task_windows` / `service_windows`）はコンパイルされない。`detect` / `notify` / `config` / `state` はタグなしでクロスプラットフォームにテスト可能。

## アーキテクチャ

### 2 つの独立した通知経路

ツールは補完関係にある2系統を持つ。どちらも検知ロジック（`detect`）と送信ロジック（`notify`）を共有する。

1. **起動時通知（安全網）** — タスクスケジューラの「起動時」トリガー → `run` コマンド。
   Systemイベントログを読み、**直近の起動**を分類して送信。`state.json` で多重通知を防止（1起動につき1通）。ネットワーク未確立に備え `SendWithRetry` でリトライ。
   登録は [task_windows.go](task_windows.go)（`schtasks` + UTF-16LEのXML定義、SYSTEM・最上位権限・1分遅延）。

2. **シャットダウン前通知** — Windowsサービス `restart-message-svc` → SCMの **PRESHUTDOWN** 通知 → `sendShutdownNotice`。
   **進行中の**シャットダウンを直近のEvent 1074から分類。短いタイムアウト＋少数回リトライで、シャットダウンを長く止めない。
   実装は [service_windows.go](service_windows.go)（`golang.org/x/sys/windows/svc`）。手動確認用に `shutdown-notice` コマンドあり。

### パッケージの責務

- [main.go](main.go) — サブコマンドのディスパッチと**表示整形のみ**（Discord embedの構築 `buildMessage` / `buildPendingMessage`、カテゴリ→ラベル/色/絵文字の対応 `present`、`collect` によるブート検知のリトライ）。
- [internal/winevent/](internal/winevent/) — `wevtapi.dll` をネイティブ呼び出しでイベントログを読む。**`wevtutil`/PowerShell をシェルアウトしない**設計（依存削減＋コンソールのコードページ破損回避。UTF-16を明示デコード）。
- [internal/detect/](internal/detect/) — **本体の心臓部**。イベントログ群から最新ブートを分類する（`Latest`）／進行中シャットダウンを分類する（`PendingShutdown`）。
- [internal/notify/](internal/notify/) — Discord Webhook送信。`HTTPError.Permanent()` で永続エラー（429以外の4xx）はリトライ打ち切り。**Webhook URL の秘密トークンをエラーに漏らさない**（`scrubURLError` が `*url.Error` を全層剥がす）。
- [internal/config/](internal/config/) — 設定の読込。既定値 → 設定ファイル → 環境変数 `DISCORD_WEBHOOK_URL` の順で上書き。
- [internal/state/](internal/state/) — 通知済み状態の永続化（temp + renameのアトミック書込）。

### 検知ロジックの重要な不変条件（`detect` を編集する前に必読）

- **ロケール非依存**で判定する。表示言語に依存しないシグナル（1074の理由コード `param4`・開始プロセス `param1`、6008、Kernel-Power 41の `BugcheckCode`）を主に使う。文字列マッチは補助。
- すべてのイベント対応付けは**現在のブートサイクル内**にスコープする（前回ブートマーカーと今回ブートの間）。古い1074を最新ブートに誤って紐付けないため。
- **分類の順序が重要**: 電源オフ判定（`isPowerOff`）はUpdateヒューリスティック（`isUpdate`）より**先**に評価する。計画済み・SYSTEM起因の完全シャットダウンを誤って `update` にしないため。
- Event 41（Kernel-Power）**単独では「予期しない」と判定しない**。通常再起動でも記録される（`BugcheckCode == 0`）。`crash` は `BugcheckCode != 0` のときのみ。
- カテゴリは `update` / `manual` / `shutdown` / `unexpected` / `crash` / `unknown`。新カテゴリ追加時は `main.go` の `present` にラベル/色/絵文字も追加する。

### 設定・状態の探索パス

- 設定: `-config` → `RESTART_MESSAGE_CONFIG` → 実行ファイル隣の `config.json` → `%ProgramData%\restart-message\config.json`。Webhook URLは `DISCORD_WEBHOOK_URL` で上書き可。
- 状態・サービスログ: `%ProgramData%\restart-message\`（`state.json` / `service.log`）。SYSTEM/サービスからも読める場所を使う。
- `config.json` は秘密情報を含むため `.gitignore` 済み。テンプレートは `config.example.json`。

## ブランチ運用とリリース

- **git-flow (AVH Edition)**: `main`（安定版）/ `develop`（統合）/ `feature/*`・`release/*`・`hotfix/*`。PRの既定ベースは `develop`。
- リリースはGitHub Releaseをpublishすると [release.yml](.github/workflows/release.yml) がバイナリ・`SHA256SUMS.txt`・provenanceを自動添付。バージョンは `-ldflags "-X main.Version=<tag>"` で埋め込む（既定は `main.go` の `Version`）。
- CI: [codeql.yml](.github/workflows/codeql.yml)（Windowsランナーでネイティブビルドして解析）、Dependabot（PRは `develop` 宛）。
