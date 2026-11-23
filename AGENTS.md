# 開発ガイド

## 基本方針

- 作業報告は日本語で簡潔かつ丁寧に書く。
- 改行・空行・インデントは [コーディング規約](docs/coding-style.md) に従う。
- Go、`net/http`、`html/template`、素のJavaScript、PostgreSQLで構成する。Goのバージョンは `go.mod` を参照する。
- モジュラーモノリスを維持する。依存方向とDBの所有境界は [設計方針](docs/architecture.md) に従う。依存方向は `architecture/dependencies_test.go` で検査する。
- `feeds` は購読とフォルダー、`articles` は記事と閲覧状態を所有する。取得の調整は `ingestion`、検索の入口は `search`、HTTP・画面・コマンドの接続は `cmd/reader` に置く。
- DBへのアクセスは所有モジュールのストアに置く。Webと取得コマンドは `FEEDS_DATABASE_URL` と `ARTICLES_DATABASE_URL` を使い、管理者用の `DATABASE_URL` はマイグレーションに使う。

## 実行と検証

```sh
docker compose up --build -d
go test ./...
node --check cmd/reader/reader.js
docker compose --profile test run --build --rm test
```

- 通常のCompose起動はDB、マイグレーション、Webの順に実行する。`docker compose up --build --watch` の監視対象は `cmd/`、`internal/`、`go.mod`、`go.sum`。マイグレーションを追加したら通常の起動コマンドで適用する。
- スキーマ変更は既存のSQLファイルを変更せず、`migrations/` に次の番号のファイルを追加する。統合テストは実PostgreSQL上に専用DBを作り、終了時にそのDBを削除する。
- 自動取得は `AUTO_REFRESH`、基本間隔は `FETCH_INTERVAL` で設定する。取得間隔は基本値とフィードのTTLの長い方を使う。定期実行と1回分の処理を分け、Webと `reader fetch --due` で共有する。
- 開発用の設定例は `.env.example`。実際の `.env`、認証情報、DBダンプ、個人の購読OPMLをGitに追加しない。DBの初期パスワード設定は空のボリュームに対して実行される。

## Kubernetes へのデプロイ

- Docker Compose はローカル開発・検証に使う。開発した変更は検証後、Kubernetes の `rss-reader` 名前空間にデプロイする。
- `make publish` でイメージを GHCR に登録し、`k8s/reader/` のイメージタグを更新する。デプロイ後はロールアウトの完了と `/healthz`、変更した機能の動作を確認する。
- DB のスキーマを変更した場合は、既存データでの動作と稼働中の Web との互換性を検証し、Kubernetes の `reader` DB をバックアップして復元手順を確認する。稼働中の Web と互換性がない場合は、先に書き込み元を停止する。
- 新しいスキーマは `reader migrate` の Job で適用する。Job の成功を確認してから新しい Web を起動する。

## 変更時に守る挙動

- 購読解除後も記事を保持し、フィード再取得で既読・お気に入り・取得済み全文を上書きしない。OPMLの取り込みは一括で反映する。
- 記事一覧には表示に必要なメタデータを返し、記事を開く操作では右ペインを部分更新する。未読一覧で既読にした行は、その場でグレーアウトして順序を保つ。自動ページ送りでは読み込み済みの行に続きを追加する。
- フォルダーの並び順と開閉はDBに保存する。ブラウザーの永続ストレージに依存させない。
- 外部から取得した記事HTMLは許可リストでサニタイズする。DB管理画面（`/admin/database`）はアプリ所有テーブルの読み取り専用とする。
