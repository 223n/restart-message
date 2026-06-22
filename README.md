# restart-message

Windows が再起動（Windows Update・手動・予期しないシャットダウン等）した際に、
起動時へ Discord Webhook で通知を送るツールです。

- 言語: Go（単一バイナリ、ランタイム不要）
- 起動契機: Windows タスクスケジューラ（起動時 / ログオン時）
- 検知方式: Windows イベントログ（System ログ）
- 通知先: Discord Webhook

詳細なセットアップ手順は開発完了後に追記します。
