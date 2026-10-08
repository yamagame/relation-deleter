# Research & Design Decisions

## Summary
- **Feature**: `relation-deleter`
- **Discovery Scope**: New Feature（greenfield。full discovery を実施）
- **Key Findings**:
  - `github.com/go-sql-driver/mysql` の最新版 v1.10.x は **Go 1.24 以上**が必要。Go 1.24+ を前提にする。開発環境は asdf で Go 1.27.1 をプロジェクトに固定した（`.tool-versions`）
  - `JSON_ARRAYAGG` は集約の順序を保証しない。`GROUP_CONCAT(JSON_OBJECT(...) ORDER BY ... )` で配列を組み立てれば、出力が決定的になり（ゴールデンテスト・差分レビューが可能）、必要なバージョンも MySQL 5.7.8+（`JSON_OBJECT`）まで下がる
  - `SET SESSION foreign_key_checks = 0` は特権なしで実行できる（制限付きセッション変数ではない）。循環参照のあるテーブル群を削除するときに使える
  - `MYSQL_PWD` は非推奨（`ps` で他ユーザーに見える環境がある）。スクリプトは一時的な `--defaults-extra-file`（mode 600）に変換して渡す

## Research Log

### Go MySQL ドライバのバージョン
- **Context**: steering では Go 1.22+ としていたため、依存の最低 Go バージョンを確認した
- **Sources Consulted**: pkg.go.dev `github.com/go-sql-driver/mysql@v1.10.1`、リポジトリのコミット #1763（サポート範囲 1.24–1.26）
- **Findings**: v1.10.1（2026-09）は Go 1.24+ が必要。v1.10.0 は 2026-04 リリース
- **Implications**: `go.mod` は `go 1.24` にする。開発環境は asdf で golang 1.27.1 に更新済み（2026-10-08）。tech.md も更新した

### information_schema からの決定的な JSON 出力
- **Context**: スキーマファイルをバージョン管理・ゴールデンテストするには、出力の順序を安定させる必要がある
- **Sources Consulted**: MySQL 8.0 Reference Manual（JSON 関数、GROUP_CONCAT、INFORMATION_SCHEMA TABLES / COLUMNS / KEY_COLUMN_USAGE / REFERENTIAL_CONSTRAINTS）
- **Findings**:
  - `JSON_ARRAYAGG` には `ORDER BY` を指定できず、要素の順序は保証されない
  - `GROUP_CONCAT(expr ORDER BY ... SEPARATOR ',')` なら順序を指定できる。上限は `group_concat_max_len` で決まり、セッション単位で引き上げられる
  - `JSON_OBJECT` はキー順を正規化し、文字列中の改行もエスケープするので、1行1レコードで出力できる
  - `mysql -N -B -r` を付けると、ヘッダなし・タブ区切り・生出力になる（バックスラッシュのエスケープなし）
  - KEY_COLUMN_USAGE の `ORDINAL_POSITION` で複合キーのカラム順が分かる。`REFERENCED_TABLE_SCHEMA` で別スキーマを参照する FK を判別できる
- **Implications**: トップレベルの配列は、テーブルごと・FK ごとに1行ずつ SELECT し、bash で連結する。巨大な単一値による `max_allowed_packet` の問題を避けられる

### 認証情報の受け渡し
- **Context**: 要件 9.1 / 9.2
- **Sources Consulted**: MySQL 8.4 / 9.0 Manual「Environment Variables」、WL#13449
- **Findings**: `MYSQL_PWD` は deprecated だが、まだ削除はされていない。推奨は option file（`--defaults-extra-file`）
- **Implications**: スクリプトは `MYSQL_PWD` を受け取ったら一時 option file に書き出して `unset` し、`trap` で削除する。Go CLI は `MYSQL_PWD` 環境変数と option file の `[client]` セクション（user / password / host / port / socket）の両方を読める

### foreign_key_checks の扱い
- **Context**: 要件 6.6（自己参照・循環参照があっても全件削除する）
- **Sources Consulted**: MySQL 8.0 Manual「System Variable Privileges」、Bug #92032
- **Findings**: session の `foreign_key_checks` は誰でも変更できる。トランザクションの途中でも変更できる。チェックを無効にしている間は ON DELETE CASCADE / SET NULL も発動しない
- **Implications**: 強連結成分（循環を含むテーブル群）を削除するときだけ無効にし、直後に必ず有効へ戻す。削除対象は参照の閉包なので、コミット時点で宙に浮いた参照は残らない（同時挿入を除く。Risks を参照）

## Architecture Pattern Evaluation

| Option | Description | Strengths | Risks / Limitations | Notes |
|--------|-------------|-----------|---------------------|-------|
| パイプライン（採用） | 読込 → グラフ → 収集 → 計画 → 実行/表示。各段は純粋なデータを受け渡す | 各段を単体テストできる。dry-run と execute が計画を共有できる | 段の数だけパッケージが増える | steering の依存方向と一致 |
| JOIN 付き DELETE の生成 | 収集せずに多段 JOIN で削除する | 実装が小さい | 対象一覧を表示できない。循環に弱い | discovery で不採用 |
| ON DELETE CASCADE への依存 | DB の CASCADE に任せる | 実装不要 | FK のない関連には効かない。件数検証ができない | 要件 4.2 と矛盾 |

## Design Decisions

### Decision: 削除順序は SCC の逆トポロジカル順にし、循環群だけ FK チェックを無効化する
- **Context**: 要件 6.5 / 6.6
- **Alternatives Considered**:
  1. 削除全体で `foreign_key_checks=0` にする
  2. 行単位で依存順を求める
  3. SCC 単位で順序付けし、非自明な SCC（サイズ2以上、または自己ループ）の間だけ無効化する
- **Selected Approach**: 3
- **Rationale**: 通常のテーブルでは DB の整合性チェックが安全網として残る。行単位の順序付けは複雑すぎる
- **Trade-offs**: 循環群の内部では DB のチェックが効かない。ただし削除対象は閉包なので、整合性は保たれる
- **Follow-up**: 結合テストで、自己参照ツリーと2テーブルの相互参照を検証する

### Decision: レコードの識別子は「PK タプル」、PK のないテーブルは「全カラムタプル＋重複数」
- **Context**: 要件 4.3（重複して数えない）、4.4（PK なしテーブル）、6.9（件数検証）
- **Alternatives Considered**:
  1. PK のないテーブルは件数だけを数える → 複数の関連から重複して数えてしまう
  2. 全カラムを `GROUP BY` してタプルとその件数を得る
- **Selected Approach**: 2。削除は関連ごとの参照カラム述語で行う
- **Trade-offs**: BLOB / TEXT の `GROUP BY` は `max_sort_length` で切り詰められるので、PK のないテーブルに長大な BLOB が同値で並ぶと件数が不正確になりうる。その場合は件数検証（6.9）で検出してロールバックする

### Decision: 値の型は、スキーマのカラム型で string と []byte を切り替える
- **Context**: 収集した値を次の段の述語に再利用する。BINARY 系の値を文字列として渡すと、utf8mb4 の不正バイトエラーになる
- **Selected Approach**: `binary` / `varbinary` / `*blob` / `bit` は `[]byte`、それ以外は `string` として扱う。NULL を含むタプルは参照を辿らない
- **Rationale**: 文字列は接続照合順序のまま比較させ、FK の照合と同じ挙動にする

### Decision: 収集と削除を同じトランザクションで行う
- **Context**: 要件 4.6 / 5.1 / 6.5 / 6.9
- **Selected Approach**: dry-run は `READ ONLY` トランザクションで収集し、最後にロールバックする。execute は1つの読み書きトランザクションで「収集 → 確認 → 削除 → 検証 → コミット」を行う
- **Trade-offs**: 確認待ちの間もトランザクションが開いたままになる。通常の SELECT ならロックは取らないので、影響は undo の保持だけ

### Generalization
- 「起点の検証」「子レコードの収集」「PK なしテーブル」は、いずれも **述語（テーブル＋カラム列＋値タプル集合）でレコードを取得する** という同じ操作の変形にまとまる。`RowSource.Fetch(table, cols, predicate)` という単一の口に一般化した
- dry-run と execute は同じ `Plan` を共有する。違いは、表示するか実行するかだけ

### Build vs Adopt
- ドライバ: `go-sql-driver/mysql` を採用（デファクトで、MPL-2.0）
- YAML: `go.yaml.in/yaml/v3` を採用（`yaml.Node` の行番号を、エラー箇所の表示に使う）
- option file のパース: 自作する。`[client]` セクションの key=value だけを読む。INI ライブラリを入れるほどではない
- SCC: Tarjan 法を自作する（数十行で済み、依存を増やす理由がない）
- TTY 判定: 標準ライブラリの `os.File.Stat()` の `ModeCharDevice` で判定する。`x/term` は不要
- sqlmock は採用しない。`RowSource` / `Execer` の interface をフェイクで差し替えてテストする

### Simplification
- 出力形式は人が読むテキストのみ（JSON 出力は要件にない）
- 削除起点の指定は `--table` と `--id` の繰り返しに限定（ファイル入力はなし）
- dump は Go から呼ばない。スクリプトと CLI はスキーマファイルでだけつながる

## Risks & Mitigations
- 収集から削除までの間に子レコードが同時に挿入される → FK があれば削除エラーになりロールバックされる。FK のない関連では孤児が残りうる。この点をドキュメントに明記する
- 別スキーマのテーブルから参照されている → スキーマファイルには現れない。FK エラーでロールバックされるので安全側に倒れる
- プレースホルダは1文あたり 65,535 個が上限 → チャンクサイズ × カラム数がこれを超えないように、チャンクサイズを自動で縮める
- Go 1.24 未満の環境 → `go.mod` の go ディレクティブでビルド時に検出できる。README / tech.md に明記する
- 対象が数万件あると dry-run の出力が巨大になる → 標準出力に全件を出す（要件 5.2）。進捗は標準エラーに分離する

## References
- [go-sql-driver/mysql v1.10.1 (pkg.go.dev)](https://pkg.go.dev/github.com/go-sql-driver/mysql@v1.10.1) — Go 1.24+ が必要
- [MySQL 8.4 Environment Variables](https://dev.mysql.com/doc/refman/8.4/en/environment-variables.html) — MYSQL_PWD は deprecated
- [WL#13449](https://dev.mysql.com/worklog/task/?id=13449) — MYSQL_PWD の非推奨化
- [MySQL 8.0 System Variable Privileges](https://docs.oracle.com/cd/E17952_01/mysql-8.0-en/system-variable-privileges.html) — セッション変数の権限
- [Bug #92032](https://bugs.mysql.com/92032) — session の foreign_key_checks は特権なしで変更できる
