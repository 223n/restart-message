# restart-message

[![CI](https://img.shields.io/github/actions/workflow/status/223n/restart-message/ci.yml?branch=main&label=CI)](https://github.com/223n/restart-message/actions/workflows/ci.yml)
[![CodeQL](https://img.shields.io/github/actions/workflow/status/223n/restart-message/codeql.yml?branch=main&label=CodeQL)](https://github.com/223n/restart-message/actions/workflows/codeql.yml)
[![Release](https://img.shields.io/github/v/release/223n/restart-message?label=release)](https://github.com/223n/restart-message/releases/latest)
[![License](https://img.shields.io/github/license/223n/restart-message?label=license)](LICENSE)
[![Go](https://img.shields.io/github/go-mod/go-version/223n/restart-message?label=go)](go.mod)

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

- Windows 10 / 11（x64 / ARM64）
  - x64（Intel / AMD）の環境では `amd64` 版を、ARM64の環境（Snapdragon搭載PCなど）では
    `arm64` 版を使用します。
  - Windows 11 on ARM は x64 のエミュレーションに対応しているため、ARM64の環境でも
    `amd64` 版は動作します。ただし、ネイティブで動く `arm64` 版を推奨します。
- ビルド時のみ: Go 1.26.8以降

## ダウンロードと検証

ビルド済みバイナリは [Releases](https://github.com/223n/restart-message/releases) から入手できます。
各リリースには次の4ファイルが添付されます（`<version>` はリリースのタグ名です）。

| ファイル                                      | 対象                                       |
|-----------------------------------------------|--------------------------------------------|
| `restart-message_<version>_windows_amd64.exe` | x64（Intel / AMD）のWindows                |
| `restart-message_<version>_windows_arm64.exe` | ARM64（Snapdragon搭載PCなど）のWindows     |
| `SHA256SUMS.txt`                              | 上記2つの実行ファイルのSHA256ハッシュ一覧  |
| `THIRD_PARTY_NOTICES.txt`                     | 実行ファイルに含まれる第三者ライセンス表示 |

どちらを選ぶか分からない場合は、PowerShellで確認できます。
`AMD64` と表示されたら `amd64` 版、`ARM64` と表示されたら `arm64` 版です。

```powershell
$env:PROCESSOR_ARCHITECTURE
```

ダウンロード後、SHA256が一致することを確認してください。
ファイル名にはバージョンが入るため、以降のコマンドではワイルドカードで受けた変数を使い回します。
同じPowerShellセッションのまま、続けて実行してください。

```powershell
# ARM64の環境では amd64 を arm64 に読み替えてください。
$exe  = (Get-Item .\restart-message_*_windows_amd64.exe).FullName
$hash = (Get-FileHash $exe -Algorithm SHA256).Hash
$line = Select-String -Path .\SHA256SUMS.txt -SimpleMatch -Pattern (Split-Path $exe -Leaf)
# 行が見つからないまま比較すると $want が空文字になり、「MISMATCH: <hash> / 」という
# 一致しなかったのか一覧に載っていないのか分からない表示になる。先に切り分ける。
if (-not $line) { "NOT LISTED: $(Split-Path $exe -Leaf) は SHA256SUMS.txt にありません" }
else {
    $want = ($line.Line -split '\s+')[0]
    if ($hash -ieq $want) { "OK: $hash" } else { "MISMATCH: $hash / $want" }
}
```

`OK:` に続けてハッシュが表示されれば一致です。`Get-FileHash` は大文字、`SHA256SUMS.txt` は
小文字でハッシュを出力するため、比較は大文字小文字を区別しない `-ieq` で行っています。

ビルド来歴（provenance）も検証できます（GitHub CLIが必要）。

```powershell
gh attestation verify $exe --repo 223n/restart-message
```

## ビルド

```powershell
pwsh -File scripts/build.ps1
# 生成物: bin\restart-message.exe
```

ARM64版をビルドする場合は `-Arch` を指定します。出力先の名前が変わるため、
タスク登録時は `install-task.ps1` に `-ExePath` で明示してください。

```powershell
pwsh -File scripts/build.ps1 -Arch arm64
# 生成物: bin\restart-message_arm64.exe
```

`-Version` を省略すると、`main.go` の `Version` の値がそのまま使われます。
リリース時のバージョンは `release.yml` が埋め込むため、通常は指定不要です。

手動でビルドする場合:

```powershell
$env:GOOS = "windows"; $env:GOARCH = "amd64"   # ARM64版は arm64
go build -trimpath -ldflags "-s -w" -o bin\restart-message.exe .
```

## 設定

`config.example.json` をコピーして `config.json` を作成します。
**Webhook URL は秘密情報のため、`config.json` は `.gitignore` 済みです。**

置き場所は用途で決まります。手元で動作確認するだけなら実行ファイルと同じフォルダーに置き、
タスクやサービスとして登録するなら `%ProgramData%\restart-message\` に置いてください（SYSTEMから読めます）。
別の場所に置いた場合は `-config` で明示します。

```powershell
# 手元での動作確認用
Copy-Item config.example.json .\bin\config.json

# 任意のパスを使う場合
.\bin\restart-message.exe status -config C:\path\to\config.json
```

```json
{
  "discord_webhook_url": "https://discord.com/api/webhooks/XXXX/YYYY",
  "username": "Restart Notifier",
  "avatar_url": "",
  "mention": "",
  "notify_on": ["update", "manual", "shutdown", "unexpected", "crash", "unknown"],
  "include_downtime": true,
  "notify_shutdown_start": true
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
  help              このヘルプを表示

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

## 実行ファイルの配置

**タスクやサービスを登録する前に、実行ファイルを管理者のみが書き込めるフォルダーへ移してください。**

`install` と `install-service` が登録するタスクとサービスは、SYSTEM・最上位の権限で動きます。
一般ユーザーが書き込めるフォルダー（`bin\`、ダウンロードフォルダーなど）に実行ファイルを置いたまま登録すると、
そのフォルダーに書き込める人が誰でもSYSTEM権限を取れる状態になります。
実行ファイルを差し替えられる場合だけではありません。設定ファイルの探索順の3番目が
「実行ファイルと同じフォルダーの `config.json`」であるため、同じフォルダーに `config.json` を
置かれるだけで、SYSTEMで動くプロセスに任意の送信先を渡せます。
詳細は [SECURITY.md](.github/SECURITY.md) を参照してください。

```powershell
# 管理者権限の PowerShell で
New-Item -ItemType Directory -Force -Path 'C:\Program Files\restart-message' | Out-Null
Copy-Item .\bin\restart-message.exe 'C:\Program Files\restart-message\restart-message.exe'
```

Releasesから入手した場合は、`restart-message_<version>_windows_amd64.exe`
（ARM64の環境では `restart-message_<version>_windows_arm64.exe`）を
`restart-message.exe` にリネームして同じ場所へ置きます。

一般ユーザーに書き込み権限がないことを確認します。

```powershell
icacls 'C:\Program Files\restart-message'
```

`BUILTIN\Users` に `(W)` や `(M)` や `(F)` が付いていないことを確認してください。

以降のインストール手順は、この配置を前提にしています。
`bin\` から直接実行してよいのは、管理者権限を必要としない `status` / `test` /
`shutdown-notice` の動作確認だけです。

## 起動時（復帰時）通知のインストール

再起動後に「PCが起動した／どんな理由で再起動したか」を通知します。
予期しないシャットダウンや強制終了も後追いで検知できる**安全網**です。

1. `config.json` を `%ProgramData%\restart-message\` に配置（SYSTEMから読めます）。
2. 管理者権限のPowerShellで登録:

    ```powershell
    & 'C:\Program Files\restart-message\restart-message.exe' install
    # または（実行ファイルを bin\ に置いたままの場合。動作確認用途に限る）
    pwsh -File scripts/install-task.ps1
    ```

登録されるタスク `restart-message` は次の設定で動作します。

- トリガー: システム起動時（ネットワーク確立のため1分の遅延）
- 実行アカウント: `SYSTEM`（最上位の特権）
- 多重起動: 抑止

アンインストール:

```powershell
& 'C:\Program Files\restart-message\restart-message.exe' uninstall
```

## シャットダウン前通知のインストール（サービス）

シャットダウン/再起動の**開始時**に「まもなく落ちます」を通知します。
Windowsサービスとして常駐し、SCMの **PRESHUTDOWN** 通知
（ネットワークは通常まだ有効）を受けて送信します。
猶予はOS既定で約3分ですが、シャットダウンを長く止めないよう、
`install-service` は登録時に60秒へ明示的に設定します。

```powershell
# 管理者権限の PowerShell で
& 'C:\Program Files\restart-message\restart-message.exe' install-service
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
& 'C:\Program Files\restart-message\restart-message.exe' uninstall-service
```

`restart-message` のサービスをすべて（旧バージョンの残骸を含めて）停止・削除します。

### 完全に削除する

`uninstall` と `uninstall-service` はタスクとサービスだけを削除します。
`%ProgramData%\restart-message\` 配下のデータは残るため、**Webhookトークンを含む
`config.json` がディスク上に残ります**。完全に削除するには次まで行ってください。

```powershell
# 管理者権限の PowerShell で
& 'C:\Program Files\restart-message\restart-message.exe' uninstall
& 'C:\Program Files\restart-message\restart-message.exe' uninstall-service
Remove-Item -Recurse -Force 'C:\ProgramData\restart-message'
Remove-Item -Recurse -Force 'C:\Program Files\restart-message'
```

最後に、Discord側で該当のWebhookを削除するか再生成してください。
`config.json` を消しても、トークンそのものが無効になるわけではありません。

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

タグ付きのリリースを公開すると、GitHub Actions（`release.yml`）が自動で
amd64 / arm64 の実行ファイルと、両方を1つにまとめた `SHA256SUMS.txt` をビルドして添付し、
ビルド来歴（provenance）の署名を付与します。

```powershell
# git-flow の release/hotfix を main へマージし、タグを push したのち。
# タグ名は main.go の Version と揃えます。
$tag = (Select-String -Path main.go -Pattern 'var Version = "(.+)"').Matches.Groups[1].Value
git tag -a $tag -m "restart-message $tag"
git push origin $tag
gh release create $tag --title "restart-message $tag" --notes-file notes.md
```

タグは `main` の先端を指すようにしてください。`release.yml` はタグを ref に
チェックアウトするため、タグの位置がそのまま配布物の中身になります。

CI:

- `ci.yml` … gofmt / vet / build / test と govulncheck（push / PR / 週次）
  - `build` は windows/arm64 のクロスコンパイルも行います（リリース時に初めて
    壊れているのを見つけないため。x64ランナー上で実行はできないためテストは回しません）
- `codeql.yml` … CodeQLによるコード解析（push / PR / 週次）
- `dependabot.yml` … 依存更新（PRは `develop` 宛）
- `release.yml` … リリース公開時のバイナリ（amd64 / arm64）・チェックサム・provenance添付
  - 添付の前に vet / test を通し、amd64 の実行ファイルを起動してバージョンを確認します
    （arm64 は x64 ランナー上で起動できないため、ビルド情報の検査だけを行います）

## ディレクトリ構成

```text
restart-message/
├─ main.go                     エントリポイント / サブコマンド / 表示整形
├─ task_windows.go             install / uninstall（起動時タスク登録）
├─ service_windows.go          service / install-service（シャットダウン前通知）
├─ internal/
│  ├─ winevent/                Windows イベントログ読取（wevtapi.dll）（+ テスト）
│  │                           record.go はビルドタグなし（Record 型・XMLデコード）
│  ├─ detect/                  再起動種別の判定（+ テスト）
│  ├─ notify/                  Discord Webhook 送信（+ テスト）
│  ├─ config/                  設定読み込み（+ テスト）
│  └─ state/                   通知済み状態の永続化（+ テスト）
├─ scripts/                    build / install / uninstall (PowerShell)
├─ .github/                    CI / CodeQL / Dependabot / Release ワークフロー, SECURITY.md
├─ config.example.json
├─ THIRD_PARTY_NOTICES.txt     第三者ソフトウェアのライセンス表示
└─ README.md
```

## セキュリティ

脆弱性の報告方法と運用上の注意（SYSTEM権限での動作、Webhook秘密情報の扱い、
実行ファイル配置による権限昇格など）は [SECURITY.md](.github/SECURITY.md) を参照してください。

## ライセンス

[MIT](LICENSE)

同梱している第三者ソフトウェア（`golang.org/x/sys`）の著作権表示とライセンス条文は、
[THIRD_PARTY_NOTICES.txt](THIRD_PARTY_NOTICES.txt) にまとめています。
