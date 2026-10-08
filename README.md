# mysql-relation-deleter

MySQL のレコードを、それを参照している子孫レコードごと、正しい順序で安全に削除するためのツールです。

- **`scripts/dump-schema.sh`**: データベースのスキーマ（テーブル、カラム、主キー、外部キー）を JSON ファイルに書き出します。
- **`relation-deleter`**（Go 製 CLI）: スキーマファイルと手動リレーション定義をもとに、削除対象を洗い出します。削除計画を表示し、指示されたときだけ削除します。

外部キー制約のない関連も、YAML の手動リレーション定義で補えます。既定は dry-run（計画を表示するだけ）で、実際に削除するには `--execute` の指定と確認が必要です。

## 特徴

- 外部キーと手動リレーションをたどって、子孫レコードを再帰的に集めます。自己参照や循環参照があっても、探索は必ず終わります。
- `ON DELETE` のルール（CASCADE / SET NULL / RESTRICT）にかかわらず、参照している子レコードは常に削除対象にします。
- 主キーのないテーブル、主キー以外（UNIQUE キーなど）を参照する関連、複合キー、バイナリ型のキーにも対応しています。
- 削除は1つのトランザクションで、子から親の順に行います。途中で失敗したときや、削除件数が計画と合わないときは、すべて取り消します。
- `--max-records` で件数の上限を設けられ、超えた場合は何も削除しません。
- 大量の値は IN 句をチャンクに分けて処理するので、数万件規模でも動きます。
- パスワードはコマンドライン引数で受け取らず、画面にも出力しません。
- MySQL 5.7（5.7.8 以降）と 8.0 に対応しています。

## 必要なもの

- Go 1.24 以降（開発では asdf で `.tool-versions` に書いた golang 1.27.1 を使っています）
- `mysql` コマンドラインクライアント（ダンプスクリプトが使います）
- MySQL 5.7.8 以降、または 8.0（MariaDB は対象外です）
- Docker（結合テストを動かす場合だけ）

## ビルド

```bash
go build -o ./build/relation-deleter ./cmd/relation-deleter
```

バイナリは `./build/relation-deleter` に出力されます（`build/` は `.gitignore` で除外済み）。

## 使い方

### 1. スキーマをダンプする

```bash
MYSQL_PWD='...' scripts/dump-schema.sh -h 127.0.0.1 -P 3306 -u app -D app -o schema.json
# stderr に表示: tables=16 foreign_keys=18
```

- ビューは含めず、レコードのデータも読みません。
- 出力の順序は毎回同じになるので、レビューやバージョン管理に向いています。
- ダンプに失敗したときは、既存の出力ファイルを上書きしません。
- スキーマを変更したら、ダンプし直してください。

### 2. （必要なら）手動リレーション定義を書く

外部キー制約がない関連は、YAML で定義します。

```yaml
version: 1
relations:
  - name: orders_legacy_user          # 省略時は manual#<番号>
    child:  { table: legacy_orders, columns: [customer_id] }
    parent: { table: users,         columns: [id] }
```

- 定義した関連は、子から親への外部キーと同じように扱われます。
- 複合カラムも書けます。子と親のカラムは位置で対応するので、同じ数だけ並べてください。
- 未知のキー、スキーマファイルにないテーブルやカラム、カラム数の不一致は、DB に接続する前に行番号付きのエラーになります。

### 3. 削除計画を確認する（dry-run）

```bash
MYSQL_PWD='...' ./build/relation-deleter \
  --schema schema.json --relations relations.yaml \
  --table users --id 1 \
  -h 127.0.0.1 -P 3306 -u app
```

計画は読み取り専用のトランザクションで作り、データベースは一切変更しません。出力の例を示します。

```
Deletion plan (child -> parent):
1. audit_logs: 6 rows (no primary key)
   via FK fk_audit_logs_order: audit_logs(order_id) -> orders(id)
   via FK fk_audit_logs_user: audit_logs(user_id) -> users(id)
   match order_id IN ('101','102')
   match user_id IN ('1')
2. device_tokens: 2 rows
   via FK fk_device_tokens_device: device_tokens(device_uuid) -> devices(device_uuid)
   key '1'
   key '2'
...
Statements (execution order):
DELETE FROM `audit_logs` WHERE `order_id` IN ('101','102');
...
SET SESSION foreign_key_checks = 0;
...

total=38 tables=16
```

- テーブルは削除する順に並びます。それぞれに件数、対象になった理由（`via FK <名前>` / `via manual <名前>`）、主キーの一覧が付きます。
- 実行される削除文は、値を埋め込んだ形で順に表示されます。
- 計画は stdout に、進捗と警告は stderr に出ます。

### 4. 削除する

```bash
MYSQL_PWD='...' ./build/relation-deleter --schema schema.json --relations relations.yaml \
  --table users --id 1 -h 127.0.0.1 -P 3306 -u app --execute
```

- 端末から実行すると、要約を表示してから `[y/N]` で確認します。
- スクリプトや CI から実行するときは `--yes` を付けてください。端末以外から `--yes` なしで実行すると、何もせずに終了します（終了コード 3）。
- 削除が終わると、テーブルごとの実削除件数と合計を表示します。

### 起点の指定方法

| 主キー | 例 |
|--------|----|
| 単一の主キー（複数件を指定する場合は繰り返す） | `--id 1 --id 2` |
| 複合主キー（主キーの順に CSV で書く） | `--id '101,1'`、カンマを含む値は `--id '10,"a,b"'` |
| バイナリ型の主キー | `--id 0x00ff00ff00ff00ff00ff00ff00ff00ff` |

- バイナリ型の主キーは `0x` 付きの16進で指定します。`BINARY(n)` は 0x00 で右詰めして保存されるので、n バイトすべてを書いてください。
- 同じ ID を重複して指定した場合は、1件として扱います。
- 存在しない ID は警告を出して読み飛ばします。1件も見つからなければ、終了コード 2 で終了します。

## オプション

| オプション | 説明 |
|-----------|------|
| `--schema PATH` | スキーマファイル（必須） |
| `--relations PATH` | 手動リレーション定義 |
| `--table NAME` | 起点のテーブル（必須） |
| `--id VALUE` | 起点の主キー値（必須、繰り返し可） |
| `--execute` | 削除を実行する（指定しなければ dry-run） |
| `--yes` | 確認を省略する |
| `--max-records N` | 削除対象が N 件を超えたら、削除せずに中止する（既定は 0 で、無制限） |
| `--chunk-size N` | IN 句1つあたりの値の数（既定は 500。プレースホルダの上限に合わせて自動で縮める） |
| `-h, --host` / `-P, --port` | 接続先（既定は 127.0.0.1:3306） |
| `-u, --user` | ユーザー名 |
| `-D, --database` | データベース名（既定はスキーマファイルに書かれたデータベース） |
| `--socket PATH` | Unix ソケットで接続する（host と port は使わない） |
| `--defaults-file PATH` | option file。`[client]` セクションの user / password / host / port / socket を読む |

詳しくは `relation-deleter --help` と `scripts/dump-schema.sh --help` を見てください。

## 認証情報

パスワードを指定するオプションはありません。次のどちらかで渡してください。

- 環境変数 `MYSQL_PWD`
- `--defaults-file` に指定した option file の `[client]` セクション。ファイルの権限は `chmod 600` にしてください。600 より緩いと警告が出ます。

両方を指定した場合は、`MYSQL_PWD` が優先されます。これはダンプスクリプトと CLI で共通です。ダンプスクリプトは `MYSQL_PWD` を一時的な option file（mode 600、終了時に削除）に移してから mysql を呼ぶので、パスワードがプロセスの引数に現れることはありません。

## 終了コード

### relation-deleter

| コード | 意味 |
|--------|------|
| 0 | 成功（計画を表示した、または削除が完了した） |
| 1 | 実行時エラー（接続、SQL エラー、削除の失敗、件数の不一致）。変更はすべて取り消す |
| 2 | 入力エラー（フラグ、スキーマファイル、リレーション定義、テーブルや ID の誤り、該当するレコードがない） |
| 3 | 中止（確認で拒否した、`--max-records` を超えた、端末以外から `--yes` なしで実行した） |

### dump-schema.sh

| コード | 意味 |
|--------|------|
| 0 | 成功 |
| 1 | 接続・取得・書き込みの失敗 |
| 2 | 引数の誤り、または 5.7.8 より古いサーバー |

## 注意事項と制限

- スキーマファイルは、実際のデータベースと一致させておく必要があります。一致していないと、削除時に外部キー違反などで失敗します（その場合はすべて取り消します）。
- インデックスのないカラムに手動リレーションを定義すると、照会のたびにテーブル全体を走査します。まず dry-run で所要時間を確かめてください。
- FLOAT や JSON のカラムをキーに使うと、取得した値で検索し直しても一致しないことがあります。
- 収集を始めてから削除するまでの間に、外部キーのない関連で新しく挿入された子レコードは検出しません。外部キーがある関連なら、削除がエラーになって取り消されます。
- ほかのデータベースのテーブルから参照されている場合、そのテーブルは削除対象に含まれません。
- 大文字・小文字を区別しない照合順序（`*_ci`）では、主キーのないテーブルの件数がずれることがあります。その場合は件数の検証で検出し、すべて取り消します。
- 削除前のバックアップは取りません。必要なら事前に取得してください。

## 仕組み

```
MySQL ──(dump-schema.sh)──▶ schema.json ─┐
                         relations.yaml ─┼─▶ relation-deleter ──(SELECT で収集 → DELETE)──▶ MySQL
```

1. スキーマファイルと手動定義を読み込んで検証し、参照グラフを作ります。外部キーと手動定義に同じ関連があれば、1本にまとめます。
2. 起点から子方向へ幅優先でたどり、削除対象を集めます。主キーで重複を除き、主キーのないテーブルは全カラムの値と件数で識別します。
3. テーブルを強連結成分の単位で、子から親の順に並べます。循環を含むテーブル群を削除する間だけ、セッションの `foreign_key_checks` を無効にします。
4. dry-run なら計画を表示してロールバックします。execute なら、同じトランザクションの中で削除と件数検証を行ってからコミットします。

設計の詳細は `.kiro/specs/mysql-relation-deleter/` にあります（要件、設計、タスク）。

## 開発

```bash
# 静的チェックと単体テスト
go vet ./... && go vet -tags integration ./...
go test ./...

# 結合テスト（MySQL 5.7 はポート 33057、8.0 はポート 33080 で起動する）
docker compose -f testdata/integration/docker-compose.yml up -d --wait
go test -count=1 -tags integration ./...
MRD_MYSQL_VERSIONS=8.0 go test -count=1 -tags integration ./...   # 対象のバージョンを絞る
docker compose -f testdata/integration/docker-compose.yml down -v

# ダンプスクリプトの検査
docker run --rm -v "$PWD:/mnt" -w /mnt koalaman/shellcheck:stable scripts/dump-schema.sh
```

- `mysql:5.7` の公式イメージは amd64 版しかないため、Apple Silicon ではエミュレーションで動きます。
- 結合テストのフィクスチャは `testdata/integration/fixture.sql` です。削除の起点、削除される行、残る行は、ファイル先頭のコメントにまとめてあります。
- 共有のフィクスチャ DB `app` は、テストからは読み取り専用です。データを変えるテストは `mysqltest.IsolatedDB` でテスト専用の DB を作ります。

### ディレクトリ構成

| パス | 内容 |
|------|------|
| `cmd/relation-deleter/` | エントリポイント |
| `internal/schema`, `internal/relations` | スキーマファイルと手動定義の読み込み・検証 |
| `internal/graph` | 参照グラフと削除順序 |
| `internal/collect` | 削除対象の収集 |
| `internal/plan`, `internal/execute` | 削除計画の作成と実行 |
| `internal/report` | 計画・要約・結果・進捗の表示 |
| `internal/sqlstore`, `internal/dbconn` | SQL による取得と、接続設定の解決 |
| `internal/cli` | フラグ、入力検証、dry-run と execute の流れ |
| `scripts/` | ダンプスクリプト |
| `integration/`, `testdata/` | 結合テストとテスト資材 |
