# Kubernetes運用手順

コマンドはリポジトリ直下で実行する。作業前に `kubectl config current-context` で対象クラスタを確認する。この構成では `rss-reader` 名前空間、`nfs-client` StorageClass、Sealed Secretsコントローラーを使う。WebのTailscale公開にはTailscale Operatorが必要。

## 初回構築

### 1. 名前空間と認証情報

先に名前空間を作成し、SealedSecretを適用する。別のクラスタに構築する場合は、そのクラスタのSealed Secretsコントローラー用に暗号化済みのマニフェストを用意する。

```sh
kubectl apply -f k8s/postgres/namespace.yaml
kubectl apply -f k8s/secrets/
kubectl -n rss-reader wait --for=create --timeout=120s \
  secret/postgres-credentials secret/postgres-exporter-credentials secret/ghcr-pull
```

### 2. PostgreSQL

```sh
kubectl apply -k k8s/postgres
kubectl -n rss-reader rollout status statefulset/postgres-19 --timeout=5m
```

空のデータボリュームで初めて起動するときに、DBとアプリ用ロールを初期化する。既存のボリュームを使う場合は、手元のSealedSecretが既存DBの認証情報と一致することを確認する。

### 3. スキーマとアプリ

[スキーマ変更の適用](#スキーマ変更の適用) に従い、DBのマイグレーション完了後にWebとCronJobを起動する。その後、[リリース後の確認](#リリース後の確認) を行う。

### 4. PostgreSQLの監視

PostgreSQLとスキーマの準備ができてから、監視用ロールのセットアップJobとexporterを適用する。

```sh
kubectl apply -k k8s/postgres-exporter
kubectl -n rss-reader wait --for=condition=complete --timeout=5m job/postgres-monitor-setup
kubectl -n rss-reader rollout status deployment/postgres-exporter --timeout=5m
```

exporterは `postgres-exporter` Serviceの9187番ポートにメトリクスを公開する。Prometheus側でスクレイプ対象になっていることと、`pg_up` が `1` であることを確認する。

## 通常リリース

`make publish` でイメージを発行し、`k8s/reader/kustomization.yaml` の `images.newTag` に発行したタグを設定する。差分を確認してから適用する。

```sh
kubectl diff -k k8s/reader
kubectl apply -k k8s/reader
```

この適用対象はWebのServiceとDeployment、定期取得のCronJob。DBと監視は、それぞれの設定を変更したときに個別に適用する。

## スキーマ変更の適用

`make publish` で発行し、`k8s/reader/kustomization.yaml` に設定したリリースタグを `release_tag` に指定する。同じイメージでマイグレーションJobを作成し、成功を確認してからWebとCronJobを更新する。

```sh
release_tag=20261126142820  # 今回発行したタグに置き換える
job=$(kubectl set image --local -f k8s/reader/migrate.yaml \
  "reader=ghcr.io/otknoy/rss-reader:${release_tag}" -o yaml |
  kubectl create -f - -o name) &&
kubectl -n rss-reader wait --for=condition=complete --timeout=5m "$job" &&
kubectl apply -k k8s/reader
```

Job名は作成のたびに生成される。Jobが完了しない場合は `kubectl -n rss-reader logs "$job"` で原因を確認する。

既存環境のスキーマを変える前に、DBのバックアップと復元手順を確認する。稼働中の書き込み元と新スキーマに互換性がない場合は、適用前に書き込みを止める。

## リリース後の確認

```sh
kubectl -n rss-reader rollout status deployment/rss-reader --timeout=5m
kubectl get --raw /api/v1/namespaces/rss-reader/services/http:rss-reader:8080/proxy/healthz
kubectl -n rss-reader get cronjob reader-fetch
kubectl -n rss-reader get jobs --sort-by=.metadata.creationTimestamp
```

`/healthz` が `ok` を返し、新しいイメージで起動した `reader-fetch` のJobが完了することを確認する。変更した機能もWebで確認する。
