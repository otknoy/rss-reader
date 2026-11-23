# RSS Reader

GoとPostgreSQLで動く、個人用のRSSリーダーです。RSS・Atomの自動更新、記事本文の取得、未読・お気に入り、検索、OPMLの入出力に対応しています。

## 起動

DockerとDocker Composeが必要です。

```sh
docker compose up --build -d
```

[http://localhost:8080](http://localhost:8080) を開き、「購読を管理」からフィードを登録してください。キーボード操作は `?` で確認できます。

Composeの初期設定ではローカルからアクセスできます。アプリ内の認証は未実装です。

## 開発

[開発・検証手順](AGENTS.md) · [設計方針](docs/architecture.md) · [コーディング規約](docs/coding-style.md)

## Dockerイメージの公開

`docker login ghcr.io` で認証してから `make publish` を実行すると、現在の作業ツリーから実行用イメージをビルドし、`ghcr.io/otknoy/rss-reader` に登録します。登録先のホストは `REGISTRY` で指定できます。

タグは実行時のUTC日時を `YYYYMMDDhhmmss` 形式で付けます。イメージの `org.opencontainers.image.revision` ラベルには実行時の `HEAD` を記録します。
