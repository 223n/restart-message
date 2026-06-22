# restart-message

Windows が再起動した際（Windows Update・手動・予期しないシャットダウンなど）に、
起動時へ Discord の Webhook で通知を送る軽量ツールです。

- 言語: **Go**（単一バイナリ・ランタイム不要）
- 起動契機: **タスクスケジューラ**（起動時トリガー、SYSTEM 実行）
- 検知方式: **Windows イベントログ**（System ログをネイティブ API で直接読取）
- 通知先: **Discord Webhook**

## 特徴

- 外部依存ライブラリゼロ（Go 標準ライブラリ + `wevtapi.dll` のみ）。
- イベントログを `wevtutil` / PowerShell 経由ではなくネイティブ API で読むため、
  日本語などの非 ASCII 文字が文字化けしません。
- 再起動の理由を**ロケール非依存**のシグナル（シャットダウン理由コード等）で判定。
- 状態ファイルで多重通知を防止（1 回の起動につき 1 通）。
- 起動直後でネットワーク未確立でも、送信を自動リトライ。

## 検知できる種別

| 種別 | 判定根拠（イベントログ） | 例 |
| --- | --- | --- |
| `update`（Windows Update） | Event 1074: 理由コードが「計画済 + OS」/ 開始プロセスが `TrustedInstaller.exe`・`MoUsoCoreWorker.exe` 等 | 更新による自動再起動 |
| `manual`（手動の再起動） | Event 1074: 対話ユーザーによる再起動 | スタートメニューからの再起動 |
| `shutdown`（シャットダウン→起動） | Event 1074: シャットダウン種別が電源オフ | 手動シャットダウン後の電源投入 |
| `unexpected`（予期しない） | Event 6008（前回のシャットダウンが予期しないもの） | 電源喪失・ハング |
| `crash`（ストップエラー） | Event 41(Kernel-Power) かつ `BugcheckCode != 0` | ブルースクリーン |
| `unknown`（不明） | 起動は検知したが理由を特定できず | ログのローテーション等 |

> Event 41（Kernel-Power）は通常の再起動でも記録される（`BugcheckCode = 0`）ため、
> 単独では「予期しないシャットダウン」とは判定しません。

## 必要要件

- Windows 10 / 11
- ビルド時のみ: Go 1.22 以降

## ビルド

```powershell
pwsh -File scripts/build.ps1
# 生成物: bin\restart-message.exe
```

手動でビルドする場合:

```powershell
go build -trimpath -ldflags "-s -w" -o bin\restart-message.exe .
```

## 設定

`config.example.json` をコピーして `config.json` を作成します。
**Webhook URL は秘密情報のため、`config.json` は `.gitignore` 済みです。**

```json
{
  "discord_webhook_url": "https://discord.com/api/webhooks/XXXX/YYYY",
  "username": "Restart Notifier",
  "avatar_url": "",
  "mention": "",
  "notify_on": ["update", "manual", "shutdown", "unexpected", "crash", "unknown"],
  "include_downtime": true
}
```

| キー | 説明 |
| --- | --- |
| `discord_webhook_url` | Discord の Webhook URL（必須） |
| `username` | 通知の表示名 |
| `avatar_url` | 通知アイコンの URL（任意） |
| `mention` | 先頭に付けるメンション（例: `<@&ロールID>`、`@here`） |
| `notify_on` | 通知する種別。空配列または未指定で全種別 |
| `include_downtime` | ダウンタイム（前回停止〜起動）を含めるか |

### 設定ファイルの探索順

1. `-config <path>` オプション
2. 環境変数 `RESTART_MESSAGE_CONFIG`
3. 実行ファイルと同じフォルダの `config.json`
4. `%ProgramData%\restart-message\config.json`

Webhook URL は環境変数 `DISCORD_WEBHOOK_URL` でも上書きできます。

### Discord Webhook の作成

1. Discord で通知したいチャンネルの「⚙ 設定」→「連携サービス」→「ウェブフック」
2. 「新しいウェブフック」を作成し、「ウェブフック URL をコピー」
3. その URL を `config.json` の `discord_webhook_url` に設定

## 使い方

```text
restart-message <command> [options]

  run        再起動を検知し、未通知なら Discord に通知（タスク用）
  test       設定された Webhook にテスト通知を送る
  status     検知結果を表示するだけ（送信しない）
  install    起動時に run を実行するタスクを登録（要管理者権限）
  uninstall  登録したタスクを削除（要管理者権限）
  version    バージョン表示

共通オプション:
  -config <path>   設定ファイルのパス
  -state  <path>   状態ファイルのパス
  -force           状態を無視して必ず通知する（run）
  -dry-run         送信せず内容のみ表示する（run）
```

動作確認の例:

```powershell
# 直近の起動がどう判定されるか確認（送信なし）
.\bin\restart-message.exe status

# Webhook が正しく設定されているかテスト送信
.\bin\restart-message.exe test
```

## インストール（自動起動の登録）

1. `config.json` を `%ProgramData%\restart-message\` に配置（SYSTEM から読めます）。
2. 管理者権限の PowerShell で登録:

```powershell
.\bin\restart-message.exe install
# または
pwsh -File scripts/install-task.ps1
```

登録されるタスク `restart-message` は次の設定で動作します。

- トリガー: システム起動時（ネットワーク確立のため 1 分の遅延）
- 実行アカウント: `SYSTEM`（最上位の特権）
- 多重起動: 抑止

アンインストール:

```powershell
.\bin\restart-message.exe uninstall
```

## 通知される情報

- マシン名 / 種別 / 起動時刻
- 理由（Windows が記録した理由文字列）
- 開始プロセス / 実行ユーザー / シャットダウン種別
- ダウンタイム / 理由コード / 詳細（BSOD コード等）

## 開発

このリポジトリは **git-flow**（AVH Edition）で管理しています。

- `main`: リリース済みの安定版
- `develop`: 開発統合ブランチ
- `feature/*`, `release/*`, `hotfix/*`: 作業ブランチ

テスト:

```powershell
go test ./...
go vet ./...
```

## ディレクトリ構成

```text
restart-message/
├─ main.go                     エントリポイント / サブコマンド / 表示整形
├─ task_windows.go             install / uninstall（タスク登録）
├─ internal/
│  ├─ winevent/                Windows イベントログ読取（wevtapi.dll）
│  ├─ detect/                  再起動種別の判定（+ テスト）
│  ├─ notify/                  Discord Webhook 送信（+ テスト）
│  ├─ config/                  設定読み込み
│  └─ state/                   通知済み状態の永続化
├─ scripts/                    build / install / uninstall (PowerShell)
├─ config.example.json
└─ README.md
```

## ライセンス

[MIT](LICENSE)
