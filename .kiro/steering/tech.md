# Technology Stack

> updated_at: 2026-10-08 — 実装後に同期（コマンド、終了コード、テストの規約、出力と認証の規約を追記）

## Architecture

**「ダンプ」と「削除」を、スキーマファイルという契約で分離する**構成です。

```
MySQL ──(dump script)──▶ schema.json ─┐
                      relations.yaml ─┼─▶ Go CLI ──(SELECT で収集 → DELETE)──▶ MySQL
```

- ダンプスクリプトは Go コードに依存しない。Go CLI はスキーマ解析を実行時の DB に依存しない（参照するのはダンプ結果）
- Go CLI 内部の処理の流れ: 読み込み・検証 → 参照グラフ構築 → 対象収集（読み取りのみ） → 削除計画 → 表示 or 実行
- スキーマファイルのフォーマットは両者の契約なので、変更するときは両側を同時に更新する

## Core Technologies

- **Language**: Go（CLI）、Bash（ダンプスクリプト）
- **Runtime**: Go 1.24+（ドライバ v1.10 の要件）。開発環境は asdf でプロジェクトごとに固定（`.tool-versions`: golang 1.27.1）
- **Database**: MySQL 5.7.8+ と 8.0 の両方を正式にサポートする（`JSON_OBJECT` と `GROUP_CONCAT` で順序を固定した配列を組み立てる）。どちらか一方にしかない SQL 機能（CTE、ウィンドウ関数、`utf8mb4_0900_*` 照合順序など）は使わない

## Key Libraries

- `github.com/go-sql-driver/mysql` — DB ドライバ（MPL-2.0。使うだけならライセンス上の影響はない）
- `go.yaml.in/yaml/v3` — 手動リレーション定義の読み込み（`gopkg.in/yaml.v3` はアーカイブ済みのため、こちらを使う）
- CLI の引数解析は標準ライブラリ `flag` を基本とし、外部依存は最小限にする

## Development Standards

### Code Quality
- `gofmt` / `go vet` を通すこと
- エラーは `fmt.Errorf("...: %w", err)` でラップし、ユーザー向けメッセージには原因の箇所（テーブル名、定義の行など）を含める
- パスワードを含む DSN や接続設定を、エラーやログに出力しない。接続先は `dbconn.Config.Redacted()`（`user@host:port/db`）で表示する
- CLI のエラーは `relation-deleter: ` を前置して stderr に出す。入力エラーは最初の1件で止めず、まとめて列挙する

### SQL
- 値は常にプレースホルダで渡す。識別子（テーブル名・カラム名）はスキーマファイル上に存在するものだけを、バッククォートでエスケープして使う
- 大量の値は IN 句をチャンクに分割して処理する（1文のサイズ上限対策）。既定は500件で、チャンクサイズ × カラム数がプレースホルダの上限 65,535 を超えないように自動で縮める（この定数は `plan` と `sqlstore` に重複して定義されているので、変えるときは両方直す）
- 値の型はカラム型で切り替える。バイナリ系（binary / varbinary / *blob / bit）は `[]byte`、それ以外は `string`、NULL は nil

### Testing
- 標準 `testing` パッケージとテーブル駆動テストを使う
- グラフ構築・対象収集・削除順の決定は、DB なしで単体テストできるように設計する
- DB を使う結合テストは Docker の MySQL 5.7 と 8.0 の両方で行い、build tag で通常のテストと分ける。`mysql:5.7` は amd64 イメージのみなので `platform: linux/amd64` を指定する
- ダンプスクリプトは、テスト用スキーマに対する出力をゴールデンファイルと比較して検証する（バージョンごとに異なる `server_version` だけを正規化して、残りはバイト単位で比較する）
- 共有のフィクスチャ DB `app` はテストからは読み取り専用。データを変えるテストは `mysqltest.IsolatedDB` でテスト専用の DB を使う（パッケージは並列に実行されるため）
- フェイクは実物の挙動に合わせる（例: Execer のフェイクは、キャンセルされた ctx では実行を拒否する）。テストが本当に不具合を検出できるかは、ミューテーション（実装をわざと壊す）で確かめる

## Development Environment

### Required Tools
- Go 1.24+（asdf で `.tool-versions` の版を使う）、`mysql` クライアント 8.0、Docker（結合テスト用）
- asdf の golang プラグインが古く、macOS の `/sbin/sha256sum` で検証に失敗する場合は `PATH="/opt/homebrew/bin:$PATH" asdf install golang <ver>` を使う

### Common Commands
```bash
# Vet:         go vet ./... && go vet -tags integration ./...
# Unit test:   go test ./...
# DB 起動:     docker compose -f testdata/integration/docker-compose.yml up -d --wait   # 5.7: 33057, 8.0: 33080
# 結合テスト:   go test -count=1 -tags integration ./...      # MRD_MYSQL_VERSIONS=8.0 で対象を絞れる
# Build:       go build -o ./build/relation-deleter ./cmd/relation-deleter   # build/ は .gitignore 済み
# Shellcheck:  docker run --rm -v "$PWD:/mnt" -w /mnt koalaman/shellcheck:stable scripts/dump-schema.sh
```
成果物の出力先は `./build/` に統一する。`go build ./...` だけを実行すると、リポジトリ直下にバイナリが出力されることがあるので、必ず `-o` で出力先を指定する。

## Key Technical Decisions

- **スキーマの事前ダンプ**: 削除時に `information_schema` を直接見るのではなく、ダンプ済みのファイルを入力にする。理由は、レビューとバージョン管理ができ、手動定義をそのファイルに対して検証でき、本番 DB への問い合わせも減るため
- **PK 収集方式**: JOIN 付き DELETE は使わない。削除対象を収集してから削除する。理由は、dry-run で対象一覧を正確に見せられ、循環参照を訪問済み管理で扱えるため
- **ON DELETE は無視して常に削除**: ルールごとの挙動の切り替えはしない。挙動を単純で予測可能に保つため
- **認証情報**: 環境変数か接続設定ファイル（`--defaults-extra-file` 相当）で渡す。プロセス引数には出さない。両方が指定されたら `MYSQL_PWD` を優先する（スクリプトと CLI で同じ）
- **終了コード**: 0 成功、1 実行時エラー（接続、SQL、件数不一致）、2 入力エラー、3 中止（確認で拒否、件数上限の超過、非対話環境で `--yes` なし）。ダンプスクリプトは 0 成功、1 失敗、2 バージョン不足・引数誤り
- **トランザクション**: dry-run は READ ONLY トランザクションで収集し、必ずロールバックする。execute は1つのトランザクションで収集・確認・削除・件数検証を行い、すべて成功したときだけコミットする。BeginTx には `context.WithoutCancel` を渡す
- **循環参照**: 強連結成分の単位で子→親の順に削除する。循環を含む群を削除している間だけ、セッションの `foreign_key_checks` を無効にする（ctx がキャンセルされても必ず元に戻す）

---
_Document standards and patterns, not every dependency_
