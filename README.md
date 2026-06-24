# restart-message

Windowsが再起動した際（Windows Update・手動・予期しないシャットダウンなど）に、
DiscordのWebhookで通知を送る軽量ツールです。
次の2つの通知に対応します。

- **起動時（復帰時）通知**: タスクスケジューラで起動時に検知し通知（取りこぼしの安全網）
- **シャットダウン前通知**: Windowsサービスでシャットダウン/再起動の開始時に通知

主な構成:

- 言語: **Go**（単一バイナリ・ランタイム不要）
- 検知方式: **Windows イベントログ**（SystemログをネイティブAPIで直接読取）
- 通知先: **Discord Webhook**

## 特徴

- 依存は最小限。イベントログ読取・Discord送信はGo標準ライブラリ + `wevtapi.dll`、
  サービス制御のみ `golang.org/x/sys` を使用。
- イベントログを `wevtutil` / PowerShell経由ではなくネイティブAPIで読むため、
  日本語などの非ASCII文字が文字化けしません。
- 再起動の理由を**ロケール非依存**のシグナル（シャットダウン理由コード等）で判定。
- 状態ファイルで多重通知を防止（1回の起動につき1通）。
- 起動直後でネットワーク未確立でも、送信を自動リトライ。
- 通知のURL（Webhookの秘密トークン）をログやエラー出力に出しません。

## 検知できる種別

| 種別                               | 判定根拠（イベントログ）                                                                                 | 例                             |
|------------------------------------|----------------------------------------------------------------------------------------------------------|--------------------------------|
| `update`（Windows Update）         | Event 1074: 理由コードが「計画済 + OS」/ 開始プロセスが `TrustedInstaller.exe`・`MoUsoCoreWorker.exe` 等 | 更新による自動再起動           |
| `manual`（手動の再起動）           | Event 1074: 対話ユーザーによる再起動                                                                     | スタートメニューからの再起動   |
| `shutdown`（シャットダウン->起動） | Event 1074: シャットダウン種別が電源オフ                                                                 | 手動シャットダウン後の電源投入 |
| `unexpected`（予期しない）         | Event 6008（前回のシャットダウンが予期しないもの）                                                       | 電源喪失・ハング               |
| `crash`（ストップエラー）          | Event 41(Kernel-Power) かつ `BugcheckCode != 0`                                                          | ブルースクリーン               |
| `unknown`（不明）                  | 起動は検知したが理由を特定できず                                                                         | ログのローテーション等         |

> Event 41（Kernel-Power）は通常の再起動でも記録される（`BugcheckCode = 0`）ため、
> 単独では「予期しないシャットダウン」とは判定しません。

## 必要要件

- Windows 10 / 11
- ビルド時のみ: Go 1.25以降

## ダウンロードと検証

ビルド済みバイナリは [Releases](https://github.com/223n/restart-message/releases) から入手できます。各リリースには実行ファイルと `SHA256SUMS.txt` が添付されます。

ダウンロード後、SHA256が一致することを確認してください。

```powershell
Get-FileHash .\restart-message_0.2.1_windows_amd64.exe -Algorithm SHA256
```

ビルド来歴（provenance）も検証できます（GitHub CLIが必要）。

```powershell
gh attestation verify .\restart-message_0.2.1_windows_amd64.exe --repo 223n/restart-message
```

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

| キー                    | 説明                                                        |
|-------------------------|-------------------------------------------------------------|
| `discord_webhook_url`   | Discord の Webhook URL（必須）                              |
| `username`              | 通知の表示名                                                |
| `avatar_url`            | 通知アイコンの URL（任意）                                  |
| `mention`               | 先頭に付けるメンション（例: `<@&ロールID>`、`@here`）       |
| `notify_on`             | 通知する種別。空配列または未指定で全種別                    |
| `include_downtime`      | ダウンタイム（前回停止〜起動）を含めるか                    |
| `notify_shutdown_start` | シャットダウン前通知（サービス）を有効にするか（既定 true） |

### 設定ファイルの探索順

1. `-config <path>` オプション
2. 環境変数 `RESTART_MESSAGE_CONFIG`
3. 実行ファイルと同じフォルダーの `config.json`
4. `%ProgramData%\restart-message\config.json`

Webhook URLは、環境変数 `DISCORD_WEBHOOK_URL` でも上書きできます。

### Discord Webhook の作成

1. Discordで通知したいチャンネルの「⚙ 設定」→「連携サービス」→「ウェブフック」
2. 「新しいウェブフック」を作成し、「ウェブフックURLをコピー」
3. そのURLを `config.json` の `discord_webhook_url` に設定

## 使い方

```text
restart-message <command> [options]

  run               再起動を検知し、未通知なら Discord に通知（タスク用）
  test              設定された Webhook にテスト通知を送る
  status            検知結果を表示するだけ（送信しない）
  install           起動時に run を実行するタスクを登録（要管理者権限）
  uninstall         登録したタスクを削除（要管理者権限）
  service           Windows サービスとして実行（SCM から起動）
  install-service   シャットダウン前通知のサービスを登録（要管理者権限）
  uninstall-service 上記サービスを削除（要管理者権限）
  shutdown-notice   シャットダウン前通知を手動で1回送る（動作確認用）
  version           バージョン表示

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

## 起動時（復帰時）通知のインストール

再起動後に「PCが起動した／どんな理由で再起動したか」を通知します。
予期しないシャットダウンや強制終了も後追いで検知できる**安全網**です。

1. `config.json` を `%ProgramData%\restart-message\` に配置（SYSTEMから読めます）。
2. 管理者権限のPowerShellで登録:

    ```powershell
    .\bin\restart-message.exe install
    # または
    pwsh -File scripts/install-task.ps1
    ```

登録されるタスク `restart-message` は次の設定で動作します。

- トリガー: システム起動時（ネットワーク確立のため1分の遅延）
- 実行アカウント: `SYSTEM`（最上位の特権）
- 多重起動: 抑止

アンインストール:

```powershell
.\bin\restart-message.exe uninstall
```

## シャットダウン前通知のインストール（サービス）

シャットダウン/再起動の**開始時**に「まもなく落ちます」を通知します。
Windowsサービスとして常駐し、SCMの **PRESHUTDOWN** 通知
（既定で約3分の猶予、ネットワークは通常まだ有効）を受けて送信します。

```powershell
# 管理者権限の PowerShell で
.\bin\restart-message.exe install-service
```

- サービス名: `restart-message-svc`（自動起動・LocalSystem）
- 再実行すると、既存の `restart-message` サービス（旧バージョンの残骸を含む）を停止・削除してから登録し直します（重複登録の防止）
- 通知後はすぐ終了し、シャットダウンを長く止めません（短いタイムアウト＋少数回リトライ）
- 直前の `Event 1074` から種別（Windows Update / 手動など）も付記（取得できた場合）

### 事前の動作確認（実際に再起動せずに1回送る）

```powershell
.\bin\restart-message.exe shutdown-notice
```

### アンインストール

```powershell
.\bin\restart-message.exe uninstall-service
```

`restart-message` のサービスをすべて（旧バージョンの残骸を含めて）停止・削除します。

> **注意（割り込みの限界）:** 電源喪失・バッテリ切れ・ブルースクリーン・
> `shutdown /f` などの強制終了では、シャットダウン前に割り込めません。
> これらは上記の「起動時（復帰時）通知」が安全網として後追いで検知します。
> 両方を有効にしておくことを推奨します。

### ログ

サービスの動作は `%ProgramData%\restart-message\service.log` に記録されます。

## 通知される情報

- マシン名 / 種別 / 起動時刻
- 理由（Windowsが記録した理由文字列）
- 開始プロセス / 実行ユーザー / シャットダウン種別
- ダウンタイム / 理由コード / 詳細（BSODコードなど）

## 開発

このリポジトリは **git-flow**（AVH Edition）で管理しています。

- `main`: リリース済みの安定版
- `develop`: 開発統合ブランチ
- `feature/*`, `release/*`, `hotfix/*`: 作業ブランチ

### テスト

```powershell
go test ./...
go vet ./...
```

### リリース

タグ付きのリリースを公開すると、GitHub Actions（`release.yml`）が自動で実行ファイルと
`SHA256SUMS.txt` をビルドして添付し、ビルド来歴（provenance）の署名を付与します。

```powershell
# 例: develop で git-flow の release/hotfix を終えてタグを push したのち
gh release create 0.2.2 --title "restart-message 0.2.2" --notes "..."
```

CI:

- `codeql.yml` … CodeQLによるコード解析（push / PR / 週次）
- `dependabot.yml` … 依存更新（PRは `develop` 宛）
- `release.yml` … リリース公開時のバイナリ・チェックサム・provenance添付

## ディレクトリ構成

```text
restart-message/
├─ main.go                     エントリポイント / サブコマンド / 表示整形
├─ task_windows.go             install / uninstall（起動時タスク登録）
├─ service_windows.go          service / install-service（シャットダウン前通知）
├─ internal/
│  ├─ winevent/                Windows イベントログ読取（wevtapi.dll）
│  ├─ detect/                  再起動種別の判定（+ テスト）
│  ├─ notify/                  Discord Webhook 送信（+ テスト）
│  ├─ config/                  設定読み込み
│  └─ state/                   通知済み状態の永続化
├─ scripts/                    build / install / uninstall (PowerShell)
├─ .github/                    CodeQL / Dependabot / Release ワークフロー, SECURITY.md
├─ config.example.json
└─ README.md
```

## セキュリティ

脆弱性の報告方法と運用上の注意（SYSTEM権限での動作、Webhook秘密情報の扱い、
実行ファイル配置による権限昇格など）は [SECURITY.md](.github/SECURITY.md) を参照してください。

## ライセンス

[MIT](LICENSE)
