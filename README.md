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

- 依存は最小限。Go標準ライブラリのほかに使うのは `golang.org/x/sys` だけです
  （イベントログはOS同梱の `wevtapi.dll` を直接呼び出して読みます）。
  `golang.org/x/sys/windows` はサービス制御のほか、その `wevtapi.dll` を
  System32から読み込む処理（`internal/winevent`）とタスク登録（`task_windows.go`）でも使います。
- イベントログを `wevtutil` / PowerShell経由ではなくネイティブAPIで読むため、
  日本語などの非ASCII文字が文字化けしません。
- 再起動の理由を**ロケール非依存**のシグナル（シャットダウン理由コード等）で判定。
- 状態ファイルで多重通知を防止（1回の起動につき1通）。
- 起動直後でネットワーク未確立でも、送信を自動リトライ。
- 通知のURL（Webhookの秘密トークン）をログやエラー出力に出しません。

## 検知できる種別

判定は上から順に行い、最初に当てはまったものを採用します。

| 種別                               | 判定根拠（イベントログ）                                                                                                                                                                   | 例                             |
|------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------------------|
| `crash`（ストップエラー）          | Event 41(Kernel-Power) かつ `BugcheckCode != 0`                                                                                                                                            | ブルースクリーン               |
| `unexpected`（予期しない）         | Event 6008（前回のシャットダウンが予期しないもの）                                                                                                                                         | 電源喪失・ハング               |
| `shutdown`（シャットダウン->起動） | Event 1074 のシャットダウン種別（`param5`）が電源オフ（`電源を切る` / `power off` / `shutdown` など）。1074が一つも残っていない場合は、同じブートサイクル内にクリーンな Event 13（または 6006）があれば同じ判定（こちらは1074の判定を全て終えたあと、`manual` より後に評価します） | 手動シャットダウン後の電源投入 |
| `update`（Windows Update）         | Event 1074 が次のいずれか。開始プロセス名に `TrustedInstaller.exe`・`MoUsoCoreWorker.exe`・`UsoClient.exe`・`wuauclt.exe`・`Windows Update` のいずれかを含む／理由コードが「計画済 + OS」／**理由コードが「計画済」かつ実行ユーザーが `SYSTEM`（プロセス名は問わない）** | 更新による自動再起動           |
| `manual`（手動の再起動）           | 上のどれにも当てはまらない Event 1074 **すべて**（受け皿。対話操作かどうかは判定していません）                                                                                             | スタートメニューからの再起動   |
| `unknown`（不明）                  | 起動は検知したが、対応する 1074 も 13/6006 も見つからず理由を特定できず                                                                                                                    | ログのローテーション等         |

> Event 41（Kernel-Power）は通常の再起動でも記録される（`BugcheckCode = 0`）ため、
> 単独では「予期しないシャットダウン」とは判定しません。

> **`manual` は「人が操作した」ことの証拠ではありません。**
> Event 1074 のうち電源オフでも `update` でもないものが、すべてここに落ちます
> （実行ユーザー `param7` を読むのは上の `update` の3番目の規則だけで、
> 「対話ユーザーが操作したかどうか」の判定には使っていません）。
> ベンダーの更新ツールやドライバーインストーラーが、SYSTEMとして「計画済」ビットなしで
> 再起動を要求した場合も `manual`（🙂 手動の再起動）と表示されます。
> したがって `notify_on: ["manual"]` は「人が再起動したときだけ通知」ではなく、
> 「分類できなかった 1074 をすべて通知」という設定になります。

> **`update` の3番目の規則はプロセス名を見ません。**
> 「計画済」ビットが立った再起動をSYSTEMが要求していれば、Windows Update以外の要求でも
> 「🔄 Windows Update」として通知されます。実際の要求元は、通知に併記される
> 「開始プロセス」で確認してください。

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

ダウンロード後、次の2つを確認してください。**先に来歴（provenance）を検証してください。**
ファイル名にはバージョンが入るため、以降のコマンドではワイルドカードで受けた変数を使い回します。
同じPowerShellセッションのまま、続けて実行してください。

```powershell
# ARM64の環境では amd64 を arm64 に読み替えてください。
$exe = (Get-Item .\restart-message_*_windows_amd64.exe).FullName
```

### 1. ビルド来歴（provenance）の検証 — 真正性の確認

これが**真正性を確認できる唯一の手段**です
（[GitHub CLI](https://cli.github.com/)・`gh auth login` 済みであること・ネットワークが必要。
`gh` は `--bundle` を使うときだけ認証なしで動きます）。

```powershell
gh attestation verify $exe --repo 223n/restart-message --signer-workflow 223n/restart-message/.github/workflows/release.yml
```

検証が通ったときに分かるのは、**そのバイトが、このリポジトリの `release.yml` で
ビルドされたものである**ということです。
それ以上のこと（バイナリの内容が安全であること、特定のバージョンであること）は示しません。

`--signer-workflow` を付けているのは、`--repo` だけでは「このリポジトリが発行元である」
ことしか固定できず、**どのワークフローが署名したかは検証されない**ためです。
現状 `attestations: write` を持つワークフローは `release.yml` だけですが、
それはこのコマンドが確かめている内容ではないので、確かめたいなら明示します。
`--signer-workflow` を知らない古い `gh` では未知のオプションとして失敗するので、
その場合は `gh` を更新してください（`--repo` だけでも検証自体は行えます）。

### 2. SHA256 の照合 — 破損・切れの確認

`SHA256SUMS.txt` との照合は、**ダウンロードが途中で切れていないか・壊れていないか**の確認です。
**リリースの資産そのものを差し替えられる攻撃者に対しては、改ざんを検知できません。**
`SHA256SUMS.txt` 自身にはハッシュも署名もなく、
provenance の対象（`release.yml` の `subject-path`）は2つの実行ファイルだけで、
この一覧ファイルは含まれていないためです。
リリースの資産を差し替えられる相手は、一覧の方も一緒に書き換えられます。
逆に、一覧まで書き換えられない相手（経路上での差し替えや壊れたミラーなど）であれば、
この照合でも食い違いに気づけます。

その代わり、`gh` もネットワークも不要で、オフラインでも実行できます。

```powershell
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

## ビルド

```powershell
pwsh -File scripts/build.ps1
# 生成物: bin\restart-message.exe
```

ARM64版をビルドする場合は `-Arch` を指定します。出力先のファイル名が変わるため、
後述の「実行ファイルの配置」でコピーするときは名前を読み替えてください。

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

> **`scripts/install-task.ps1` でタスクを登録しないでください。**
> このスクリプトは実行ファイルの `install` を呼ぶだけですが、`-ExePath` を省略すると
> リポジトリの `bin\` を指します。`bin\` は一般ユーザーが書き込めるうえ、
> 設定ファイルの探索順で「実行ファイルと同じフォルダーの `config.json`」が
> `%ProgramData%` より優先されるため、そのまま登録するとSYSTEM権限への昇格経路になります
> （[SECURITY.md](.github/SECURITY.md) 参照）。
> タスクの登録は、後述の「実行ファイルの配置」で移した実行ファイルから
> `install` を直接実行してください。

## 設定

`config.example.json` をコピーして `config.json` を作成します。
**Webhook URL は秘密情報のため、`config.json` は `.gitignore` 済みです。**

置き場所は用途で決まります。手元で動作確認するだけなら実行ファイルと同じフォルダーに置き、
タスクやサービスとして登録するなら `%ProgramData%\restart-message\` に置いてください（SYSTEMから読めます）。
別の場所に置いた場合は `-config` で明示します。

**`%ProgramData%\restart-message\` に置く場合は、アクセス権を自分で設定してください。**
既定のままでは、Webhookトークンを含む `config.json` を**そのPCの一般ユーザー全員が読めます**
（設定手順は後述の「起動時（復帰時）通知のインストール」にあります）。

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
| `discord_webhook_url`   | Discord の Webhook URL（必須。`https` のみ受け付けます）    |
| `username`              | 通知の表示名                                                |
| `avatar_url`            | 通知アイコンの URL（任意）                                  |
| `mention`               | 先頭に付けるメンション（例: `<@&ロールID>`、`@here`）       |
| `notify_on`             | 通知する種別。空配列または未指定で全種別                    |
| `include_downtime`      | ダウンタイム（前回停止〜起動）を含めるか                    |
| `notify_shutdown_start` | シャットダウン前通知（サービス）を有効にするか（既定 true） |

> **Webhook URL は `https` である必要があります。** トークンはURLのパスに入っているため、
> 最初の1ホップから暗号化されていなければ平文で流れます。`http://` を指定すると起動時に
> エラーになります（`127.0.0.1` などのループバック宛てだけは、動作確認用に許可します）。

### 設定ファイルの探索順

1. `-config <path>` オプション
2. 環境変数 `RESTART_MESSAGE_CONFIG`
3. 実行ファイルと同じフォルダーの `config.json`
4. `%ProgramData%\restart-message\config.json`

1と2は**操作者が明示的に指定したもの**なので、指したファイルが読めなければエラーになります
（存在しない、ディレクトリである、権限が無い、など）。3と4はツールが自分で探す場所なので、
不在は正常として次の候補に進みます。この非対称性は意図的です。タイプミスを黙って
既定値へフォールバックさせないためです。

Webhook URLは、環境変数 `DISCORD_WEBHOOK_URL` でも上書きできます。
ただし**システム環境変数に設定したWebhook URLは、そのPCの一般ユーザー全員が読み取れ、
起動されるすべてのプロセスに引き継がれます**。常用の設定場所としては `config.json` を
推奨します（`uninstall` は環境変数を消しません。「[完全に削除する](#完全に削除する)」も参照）。

システム環境変数でも動作はします。SYSTEMで動くタスクとサービスもマシン全体の環境変数を
引き継ぐため、`install` / `install-service` はこれを見つけると
「システム環境変数として設定済みなら `-allow-unconfigured` を指定して登録する」と案内します
（ユーザー環境変数かシステム環境変数かはツール側から判別できないため、登録可否の判定からは
除外しています）。すでにその運用をしている場合は、**トークンが同じPCの全ユーザーから
読める状態であることを承知のうえで**使ってください。

### プロキシ環境について

Discordへの送信にプロキシが必要な環境では、環境変数 `HTTPS_PROXY` / `HTTP_PROXY` /
`NO_PROXY` を使用します（Goの標準的な扱い）。**Windowsのシステムプロキシ設定
（インターネット オプション / WinHTTP）は読みません。**

このうち**実際に効くのは `HTTPS_PROXY`（と除外指定の `NO_PROXY`）だけ**です。
Webhook URLは https に限られる（http はループバック宛てのみ許可）ため送信先は常に https で、
Goが `HTTP_PROXY` を参照するのは `http://` 宛てのリクエストだけだからです。
`HTTP_PROXY` だけを設定して「READMEに書いてあるのに送信できない」となる取り違えが起きます。

また、管理者のユーザー環境に `HTTPS_PROXY` があると
`restart-message test` は成功するのに、SYSTEMで動くタスクとサービスからは送信できない、
という食い違いが起こります。プロキシが必要な場合は、`HTTPS_PROXY` を
**システム環境変数**として設定してください（上のWebhook URLの注意とは別で、
プロキシのURL自体は秘密情報ではありません）。

### Discord Webhook の作成

1. Discordで通知したいチャンネルの「⚙ 設定」→「連携サービス」→「ウェブフック」
2. 「新しいウェブフック」を作成し、「ウェブフックURLをコピー」
3. そのURLを `config.json` の `discord_webhook_url` に設定（`https://` のまま貼り付けてください）

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
  shutdown-notice   シャットダウン前通知を手動で1回送る（動作確認用。既定では
                    notify_shutdown_start を尊重。-force で無視して送信）
  version           バージョン表示
  help              このヘルプを表示

共通オプション:
  -config <path>   設定ファイルのパス
  -state  <path>   状態ファイルのパス（run）
  -force           状態を無視して必ず通知する（run）
                   notify_shutdown_start が false でも送信する（shutdown-notice）
  -dry-run         送信せず内容のみ表示する（run）
  -allow-unconfigured
                   設定を解決できなくても登録する（install / install-service）
```

> ここは読みやすさのために整形した抜粋です。正確な一覧は `restart-message help` の
> 出力を参照してください。

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
`bin\` から直接実行してよいのは、`status` / `test` / `shutdown-notice` の動作確認だけです
（隣の `bin\config.json` を読むため、管理者権限は要りません。インストール先の実行ファイルで
同じ確認をする場合は、`%ProgramData%\restart-message\` のアクセス権を絞ったあとなので
管理者権限のPowerShellで実行してください）。

## 起動時（復帰時）通知のインストール

再起動後に「PCが起動した／どんな理由で再起動したか」を通知します。
予期しないシャットダウンや強制終了も後追いで検知できる**安全網**です。

1. 設定ディレクトリを作成し、**アクセス権を絞ってから** `config.json` を配置します。

    `%ProgramData%\restart-message\` は、既定では `C:\ProgramData` の権限を継承し、
    `BUILTIN\Users` に読み取り権限が付いた状態になります。
    このままではWebhookトークンを含む `config.json` を一般ユーザー全員が読めるため、
    実行ファイルの配置と同じように、明示的に権限を設定してください。

    ```powershell
    # 管理者権限の PowerShell で
    New-Item -ItemType Directory -Force -Path 'C:\ProgramData\restart-message' | Out-Null
    # 継承を切り、Administrators と SYSTEM だけに絞る。
    # 継承の解除と許可の付与は、必ず1回の icacls で行うこと。分けて実行すると
    # その間ディレクトリのDACLが空になり、再インストール時（state.json や
    # service.log をSYSTEMが作った後は、所有者がSYSTEM）に2つ目のコマンドが
    # アクセス拒否で失敗し、サービスからもツールからも使えないディレクトリが残る。
    # （管理者は所有権を取得して直せますが、原因に気づくまで通知は止まります）
    icacls 'C:\ProgramData\restart-message' /inheritance:r /grant:r 'BUILTIN\Administrators:(OI)(CI)F' 'NT AUTHORITY\SYSTEM:(OI)(CI)F'

    # 編集済みの config.json を配置（パスは手元の場所に読み替えてください）
    Copy-Item .\bin\config.json 'C:\ProgramData\restart-message\config.json'
    ```

    `restart-message` は**この設定を自動では行いません**。作成するディレクトリ・ファイルに
    ACLを設定しないため（Goの `os.MkdirAll` / `os.WriteFile` に渡すパーミッション値は
    Windowsでは読み取り専用属性以外に効果がありません）、上記は手作業の手順です。
    タスクとサービスは `LocalSystem` で動くため、`SYSTEM` への許可が必要です
    （`LocalSystem` のトークンには `Administrators` も含まれますが、
    それに頼らず明示します）。

    設定後、一般ユーザーの権限が残っていないことを確認します。

    ```powershell
    icacls 'C:\ProgramData\restart-message'
    ```

    `BUILTIN\Users` の行が出ないことを確認してください（読み取り `(R)` や `(RX)` も残さないでください）。

    権限を絞ったあとは、**`%ProgramData%\restart-message\config.json` を読むサブコマンドも
    管理者権限のPowerShellで実行してください**（インストール先の実行ファイルで動かす
    `test` / `shutdown-notice` / `status`）。一般ユーザーでは設定ファイルが
    「無い」ではなく「読めない」状態になり、`restart-message` はこれを
    （不在と違って）エラーとして扱います。読めないファイルを黙って飛ばすと、
    Webhook URL なしの既定値で動いてしまい、通知がどこにも飛ばないまま
    成功して見えるためです。`test` と `shutdown-notice` はそこで失敗し、
    `status` は警告を出したうえで検知結果だけを表示します
    （設定ファイルの欄は `(読み込み失敗 — 上の warning を参照)` になります。
    既定値が適用されたわけでもありません）。
    `bin\` の `config.json` を読む動作確認は、これまでどおり非管理者でも行えます。

2. 管理者権限のPowerShellで登録:

    ```powershell
    & 'C:\Program Files\restart-message\restart-message.exe' install
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

サービスが実際に行う判断をそのまま再現します。`notify_shutdown_start` が `false` の
場合は**送信せずにその旨を表示します**。サービスも同じ条件で送信しないため、これが
正しい確認結果です。

```powershell
# 管理者権限の PowerShell で
& "C:\Program Files\restart-message\restart-message.exe" shutdown-notice
```

設定に関わらず1通送って疎通だけを見たい場合は `-force` を付けます。

```powershell
# 管理者権限の PowerShell で
& "C:\Program Files\restart-message\restart-message.exe" shutdown-notice -force
```

> インストール先の実行ファイルで確認してください。`bin\` のコピーは隣の
> `bin\config.json` を読むため、サービスが実際に読む設定とは別のファイルになります。
>
> 管理者権限が要るのは、「起動時（復帰時）通知のインストール」の手順1で
> `%ProgramData%\restart-message\` のアクセス権を絞ったためです。
> 一般ユーザーでは `config.json` が読めず、読み取り拒否のエラーで止まります。

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

環境変数で上書きしていた場合は、それも消してください。
消し忘れると、ファイルをすべて削除した後もWebhookトークンがPCに残ります。

```powershell
# 管理者権限の PowerShell で（設定していた場合のみ）
foreach ($scope in 'Machine', 'User') {
    [Environment]::SetEnvironmentVariable('DISCORD_WEBHOOK_URL', $null, $scope)
    [Environment]::SetEnvironmentVariable('RESTART_MESSAGE_CONFIG', $null, $scope)
}
```

> `User` スコープも消しているのは、動作確認のときに一時的にユーザー環境変数として
> 設定したまま忘れている場合があるためです。`Machine` だけを消すとトークンが残ります。
> なお `User` スコープは実行した本人の分しか消えません。複数のアカウントで設定した
> 場合は、それぞれのアカウントで実行してください。

最後に、Discord側で該当のWebhookを削除するか再生成してください。
`config.json` を消しても、トークンそのものが無効になるわけではありません。

> **注意（割り込みの限界）:** 電源喪失・バッテリ切れ・ブルースクリーン・
> `shutdown /f` などの強制終了では、シャットダウン前に割り込めません。
> これらは上記の「起動時（復帰時）通知」が安全網として後追いで検知します。
> 両方を有効にしておくことを推奨します。

### ログ

サービスの動作と、**起動時通知（`run`）の結果**が
`%ProgramData%\restart-message\service.log` に記録されます。

起動時通知はタスクスケジューラからSYSTEM権限で動くためコンソールがなく、
以前は失敗しても何も残りませんでした。1回の起動につき1行が追加されます。

```text
2026-09-08 09:01:23 run: outcome=sent config=C:\ProgramData\restart-message\config.json category=update boot=2026-09-08T09:01:20Z
2026-09-08 09:01:23 run: outcome=send-failed config=C:\ProgramData\restart-message\config.json category=manual boot=2026-09-08T09:01:20Z error=Discord 送信に失敗: ...
2026-09-08 09:01:23 run: outcome=skipped-already-notified config=C:\ProgramData\restart-message\config.json category=update boot=2026-09-08T09:01:20Z
```

`config=` は**実際に読み込まれた**設定ファイルです。1つも解決できなかった場合は
`(none)` になります。誤ったインストールでは、これが最初の手がかりになります。

`outcome` は次の8種類です。設定による意図的なスキップと失敗を取り違えずに済みます。

| outcome | 意味 |
|---|---|
| `sent` | 送信し、状態も記録した（正常） |
| `sent-state-write-failed` | 送信できたが状態を記録できなかった。**次回の起動で再通知されます** |
| `skipped-already-notified` | この起動は通知済み |
| `skipped-not-in-notify_on` | `notify_on` に含まれないカテゴリ |
| `config-error` | 設定を読み込めなかった |
| `detect-error` | 起動イベントを特定できなかった |
| `no-webhook` | `discord_webhook_url` が未設定 |
| `send-failed` | Discord への送信に失敗した |

Webhook URL は記録されません。`error=` に入る文字列も、送信前に秘密トークンを
取り除いてあります。

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

`release.yml` はこれを**強制します**。タグが `main` の履歴上に無い場合、ビルドも署名も
行わずに中止します。タグを push せずにリリースを公開すると、GitHub は既定ブランチ
（`develop`）の先端にタグを作るため、未リリースの統合ブランチのコードが配布されて
しまうためです。

> リリース公開（`release: published`）で起動した場合、この検査が働く時点では
> **リリースページ自体はすでに公開されています**。止められるのは成果物の添付までで、
> 結果は「アセットが1つも付いていないリリース」になります。その場合はリリースとタグを
> 削除し、`main` の先端にタグを付け直してください。

CI:

- `ci.yml` … gofmt / vet / build / test と govulncheck（push / PR / 週次）
  - `build` は windows/arm64 のクロスコンパイルも行います（リリース時に初めて
    壊れているのを見つけないため。x64ランナー上で実行はできないためテストは回しません）
  - 週次の govulncheck は `develop` と `main` の**両方**を検査します。定期実行は既定
    ブランチでしか起動しないため、リリース済みバイナリのソースである `main` が
    一度も検査されない状態でした（脆弱性データベースは日々更新されるため、コードが
    変わらなくても答えは変わります）
- `codeql.yml` … CodeQLによるコード解析（push / PR / 週次）
  - 週次の解析も `develop` と `main` の両方を対象にします
- `dependabot.yml` … 依存更新（PRは `develop` 宛）
- `release.yml` … リリース公開時のバイナリ（amd64 / arm64）・`SHA256SUMS.txt`・
  `THIRD_PARTY_NOTICES.txt` の添付と provenance の保存
  - **タグが `main` の履歴上にあることを確認**してから先へ進みます
  - 添付の前に vet / test / **govulncheck** を通します。govulncheck だけは結果が
    「コミットの関数」ではなく「時刻の関数」なので、最後のCI以降に公表された
    標準ライブラリの脆弱性があればここで止まります
  - amd64 の実行ファイルを起動してバージョンを確認します（arm64 は x64 ランナー上で
    起動できないため、ビルド情報の検査だけを行います）

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
