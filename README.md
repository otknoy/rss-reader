# 余白 / RSS Reader

個人用のRSSリーダーのプロトタイプです。Goのモジュラーモノリスとして実装しています。

## 起動

Docker EngineとDocker Compose v2があれば起動できます。ホストへのGoやPostgreSQLのインストールは不要です。

```sh
docker compose up --build -d
```

[http://localhost:8080](http://localhost:8080) を開いてください。
DB起動 → マイグレーション → Web起動の順に実行します。

1. 「RSSフィードを登録」でRSS / AtomのURLを登録します。
2. 「記事を取得」を押すと購読中のフィードを取得します。
3. 記事の詳細表示、既読切り替え、お気に入り、検索ができます。
4. 「記事を手動で登録」からURL・タイトル・本文やメモを保存できます。

## 実装済みの機能

- RSS / Atomの購読登録・解除と一括取得、取得エラーの表示
- 記事URLの手動登録・記事削除
- 未読・既読、お気に入りと絞り込み
- 日本語タイトル・保存本文の部分一致検索、20件ごとのページ送り
- 再取得時の重複防止と状態保持、削除したRSS記事の非表示維持
- PostgreSQLへの永続保存、モジュール別DB権限

購読解除後も記事は残ります。手動登録では元サイトの本文を自動取得しません。
RSS本文はプレーンテキスト表示です。検索は保存された本文・要約が対象です。
取得先は公開HTTP / HTTPS URLに限定し、ローカル・プライベートIPへの接続を拒否します。
初版の画面は標準フォームで動作し、HTMXによる部分更新は未導入です。
認証・自動巡回・全文検索・Kubernetesマニフェストはまだ実装していません。
ローカル利用向けにWebポートを127.0.0.1へ公開しています。

## 開発

Compose Watch対応のDocker Composeでは、次のコマンドでソース変更時に再ビルドできます。

```sh
docker compose up --build --watch
```

`cmd/`、`internal/`、`go.mod`、`go.sum`を監視します。
マイグレーションを追加した場合は、通常の起動コマンドを再実行してください。
データは名前付きボリュームに残るため、コンテナを作り直しても保持されます。

```sh
# 手動でRSS取得（将来CronJobからも同じコマンドを実行可能）
docker compose run --rm web fetch

# ログ確認
docker compose logs --tail=100 web migrate

# 停止。DBのボリュームは保持する
docker compose stop
```

ポートや開発用パスワードは、必要に応じて初回起動前に`.env.example`を`.env`へコピーして変更します。
パスワードは接続URLへ埋め込むため、例のような英数字・ハイフン等のURLで安全な文字を使ってください。
DBユーザーとパスワードの初期設定は空のデータボリュームでのみ実行されます。
既存DBのパスワード変更は別途DB側の変更も必要です。付属のデフォルト値はローカル開発専用です。

## 検証

```sh
# Docker内で単体テスト・依存チェック・実DB統合テストを実行
docker compose --profile test run --build --rm test

# Go 1.27.4を利用できる場合の単体テスト・依存チェック
go test ./...
```

統合テストは専用の一時DBを作成し、終了時にそのDBだけを削除します。
通常の開発DBのデータは変更しません。
モジュール別DB権限、重複取得時の状態保持、購読解除後の検索、記事操作を検証します。

## 構成

- Go 1.27.4 / `net/http` / `html/template`
- PostgreSQL（Debian版）
- 実行用コンテナ：`gcr.io/distroless/static-debian13:nonroot`
- ビルド・テスト用コンテナ：Go公式Debianイメージ
- 実行コマンド：`web` / `fetch` / `migrate` / `healthcheck`

| モジュール | 責務 |
| --- | --- |
| `internal/feeds` | 購読・取得状況 |
| `internal/articles` | 記事・既読・お気に入り |
| `internal/ingestion` | RSS取得・解析と記事登録の調整 |
| `internal/search` | 検索、将来の全文検索への入口 |

依存ルールは [設計方針](docs/architecture.md) を参照してください。
DB接続情報は`FEEDS_DATABASE_URL`と`ARTICLES_DATABASE_URL`に分離しています。
マイグレーションだけが管理者の`DATABASE_URL`を使用します。
