# Requirements Document

## Project Description (Input)
**誰が困っているか**: MySQLデータベースを運用する開発者・運用担当者。特定のレコードを削除する際、そのレコードを参照する子レコード（さらにその子孫）を手作業で洗い出し、正しい順序で削除する必要があり、手間がかかるうえに漏れや誤削除のリスクが高い。外部キー制約が定義されていない関連も存在するため、DBの制約情報だけでは関連を網羅できない。

**現状**: スキーマ情報を取り出す仕組みも、関連を辿って削除するツールも存在しない（greenfield）。

**変えたいこと**:
- シェルスクリプト（`mysql` CLI）で `information_schema` からスキーマ（テーブル・カラム・主キー・外部キー、複合キー含む）をJSONファイルにダンプできるようにする
- Goコマンドがスキーマ JSON と手動リレーション定義（YAML、FK制約のない関連を補完）を読み込み、`--table` と `--id`（複数可）で指定したレコードを参照する子孫レコードを再帰的に収集する（循環・自己参照に対応）
- 既定は dry-run（テーブル別件数・PK一覧・DELETE文を表示するのみ）とし、`--execute` 指定時のみ単一トランザクション内で子→親の順に削除、失敗時はロールバックする
- 子のFKがNULL許容や ON DELETE SET NULL/CASCADE であっても、参照している子は常に削除する
- 大量レコードは IN 句のチャンク分割で処理し、認証情報をプロセス引数に露出させない

**対象外**: WHERE句での対象指定、ON DELETEルールに応じた挙動切替、命名規約からのリレーション推測、バックアップ/リストア、MySQL以外のDB、GUI

詳細は `brief.md` を参照。

## Introduction
本機能は、MySQLデータベースの運用者が、指定したレコードとそれを直接・間接に参照するすべての子孫レコードを、漏れなく正しい順序で安全に削除できるようにするものである。構成要素は次の2つ。
- **Schema Dump Script**: 対象データベースのスキーマ構造をスキーマファイルに書き出す
- **Relation Deleter**: スキーマファイルと手動リレーション定義をもとに削除対象を特定し、削除計画を表示する。明示的に実行を指示された場合だけ削除する

## Boundary Context
- **In scope**:
  - スキーマ構造（テーブル、カラム、主キー、外部キー）の書き出し
  - 外部キー制約のない関連の手動定義と、その検証
  - テーブル名と主キー値による削除起点の指定（複数・複合主キーを含む）
  - 子孫レコードの再帰的な特定（自己参照・循環参照を含む）
  - 既定で変更を行わないプレビュー（dry-run）
  - 明示的な実行指定・確認を経た、全件一括・全件取消可能な削除
  - 件数上限ガード
- **Out of scope**:
  - 任意の条件式による削除起点の指定
  - 削除時参照動作（SET NULL / CASCADE / RESTRICT）に応じた挙動の切り替え（子レコードは常に削除する）
  - 命名規約からの関連の自動推測
  - 削除前データのバックアップ・復元
  - スキーマの変更、削除以外のデータ操作
  - MySQL以外のデータベース、GUI
- **Adjacent expectations**:
  - 対象の MySQL は 5.7（5.7.8 以降）と 8.0 で、どちらでも同じ挙動になること
  - 運用者が、スキーマファイルを対象データベースの現行スキーマと一致する状態に保つこと（不一致による失敗は検出してロールバックするが、自動同期はしない）
  - 対象データベースに接続できる、参照・削除権限を持つアカウントが用意されていること

## Requirements

### Requirement 1: スキーマのダンプ
**Objective:** As a データベース運用者, I want 対象データベースのスキーマ構造をファイルに書き出したい, so that 削除コマンドが参照関係を把握でき、スキーマをレビュー・バージョン管理できる

#### Acceptance Criteria
1. When 運用者が接続先と対象データベースと出力先を指定してダンプを実行したとき, the Schema Dump Script shall 対象データベースのすべてのテーブル（ビューを除く）について、テーブル名、カラム（名前・型・NULL許容）、主キー構成カラム（複合主キーはカラム順を保持）を出力先のスキーマファイルに書き出す
2. When ダンプを実行したとき, the Schema Dump Script shall 対象データベースに定義されたすべての外部キー制約について、制約名、子テーブルとカラム、親テーブルとカラム（複合外部キーはカラムの対応順を保持）、削除時参照動作をスキーマファイルに含める
3. The Schema Dump Script shall テーブルのレコードデータをスキーマファイルに含めない
4. When ダンプが成功したとき, the Schema Dump Script shall 出力したテーブル数と外部キー数を表示し、終了コード0で終了する
5. If データベースへの接続、またはスキーマ情報の取得に失敗したとき, the Schema Dump Script shall エラー内容を表示して0以外の終了コードで終了し、既存の出力先ファイルを上書きも削除もしない
6. If 接続先のデータベースサーバーがダンプに必要なバージョン要件（MySQL 5.7.8 以降）を満たさないとき, the Schema Dump Script shall 必要なバージョンを示すエラーを表示して0以外の終了コードで終了する
7. The Schema Dump Script and the Relation Deleter shall MySQL 5.7（5.7.8 以降）と MySQL 8.0 のどちらに対しても、本要件のすべての受け入れ基準を満たす

### Requirement 2: 手動リレーション定義
**Objective:** As a データベース運用者, I want 外部キー制約のない関連を定義ファイルで補完したい, so that 制約のない子レコードも削除漏れなく扱える

#### Acceptance Criteria
1. Where 手動リレーション定義ファイルが指定されたとき, the Relation Deleter shall 定義された「子テーブルのカラム → 親テーブルのカラム」の関連（複合カラムを含む）を、外部キー制約と同等の参照関係として扱う
2. Where 手動リレーション定義ファイルが指定されないとき, the Relation Deleter shall スキーマファイルの外部キー制約だけで参照関係を構成する
3. If 手動リレーション定義が、スキーマファイルに存在しないテーブルやカラムを参照しているとき, the Relation Deleter shall 該当する定義箇所を示すエラーを表示し、データベースにアクセスせずに0以外の終了コードで終了する
4. If 手動リレーション定義の子側と親側でカラム数が一致しないとき, the Relation Deleter shall 該当する定義箇所を示すエラーを表示し、データベースにアクセスせずに0以外の終了コードで終了する
5. When 手動リレーション定義が、スキーマファイル上の外部キー制約と同じ関連を重複して定義しているとき, the Relation Deleter shall その関連を1つの参照関係として扱う

### Requirement 3: 削除起点の指定と入力検証
**Objective:** As a データベース運用者, I want テーブル名と主キー値で削除したいレコードを指定したい, so that 意図したレコードだけを確実に起点にできる

#### Acceptance Criteria
1. When 運用者がテーブル名と1つ以上の主キー値を指定したとき, the Relation Deleter shall 指定テーブルで主キー値が一致するレコードを削除起点とする
2. Where 指定テーブルが複合主キーを持つとき, the Relation Deleter shall 主キーの全構成カラムの値の組で各起点レコードを指定できるようにする
3. If 指定テーブルがスキーマファイルに存在しないとき, the Relation Deleter shall テーブルが見つからない旨のエラーを表示し、データベースを変更せずに0以外の終了コードで終了する
4. If 指定テーブルが主キーを持たない、または指定値の数が主キーの構成カラム数と一致しないとき, the Relation Deleter shall 入力誤りを示すエラーを表示し、データベースを変更せずに0以外の終了コードで終了する
5. If 指定した主キー値の一部がデータベースに存在しないとき, the Relation Deleter shall 存在しない主キー値を警告として表示し、存在するレコードだけを起点に処理を続ける
6. If 指定した主キー値がいずれもデータベースに存在しないとき, the Relation Deleter shall 対象レコードがない旨を表示し、データベースを変更せずに0以外の終了コードで終了する
7. If スキーマファイルを読み込めない、または形式が不正なとき, the Relation Deleter shall 原因を示すエラーを表示し、データベースにアクセスせずに0以外の終了コードで終了する

### Requirement 4: 子孫レコードの特定
**Objective:** As a データベース運用者, I want 起点レコードを直接・間接に参照するすべてのレコードを自動で特定したい, so that 手作業の洗い出しをせずに削除漏れを防げる

#### Acceptance Criteria
1. When 削除起点が確定したとき, the Relation Deleter shall 外部キー制約と手動リレーション定義の両方をたどって、起点レコードを参照するレコードを再帰的にすべて削除対象に加える
2. The Relation Deleter shall 子レコード側の参照カラムがNULL許容であるか、どの削除時参照動作が定義されているかにかかわらず、参照している子レコードを削除対象に加える
3. When 参照関係に自己参照や循環参照が含まれるとき, the Relation Deleter shall 同じレコードを重複して数えず、探索を必ず終了させる
4. When 子テーブルが主キーを持たないとき, the Relation Deleter shall そのテーブルのレコードを参照カラムの値で削除対象に含め、その件数を削除計画に表示する
5. The Relation Deleter shall 主キー以外のカラムを参照先とする関連についても、参照先カラムの値をもとに子レコードをたどる
6. While 削除対象を特定している間, the Relation Deleter shall データベースの内容を変更しない

### Requirement 5: 削除計画のプレビュー（dry-run）
**Objective:** As a データベース運用者, I want 実際に削除する前に、何が消えるかを確認したい, so that 誤削除を防げる

#### Acceptance Criteria
1. While 運用者が削除の実行を明示的に指定していない間, the Relation Deleter shall データベースを一切変更せず、削除計画を表示するだけで終了する
2. When 削除計画を表示するとき, the Relation Deleter shall 削除順に、テーブルごとの削除対象件数と、削除対象レコードの主キー値（主キーのないテーブルは参照カラムの値）を表示する
3. When 削除計画を表示するとき, the Relation Deleter shall 実行時に発行する削除文を実行順に表示する
4. When 削除計画を表示するとき, the Relation Deleter shall 削除対象の総件数と、関係するテーブル数を表示する
5. When 削除計画を表示するとき, the Relation Deleter shall 各テーブルがどの関連（外部キー制約か手動定義か）によって削除対象になったかを判別できる情報を表示する

### Requirement 6: 削除の実行
**Objective:** As a データベース運用者, I want 確認した削除計画どおりに、全件まとめて安全に削除したい, so that 中途半端な削除状態を残さずに済む

#### Acceptance Criteria
1. When 運用者が削除の実行を明示的に指定したとき, the Relation Deleter shall 削除計画の要約（テーブルごとの件数と総件数）を表示し、運用者の確認を求める
2. If 確認で運用者が承認しなかったとき, the Relation Deleter shall データベースを変更せずに終了する
3. Where 運用者が確認の省略を明示的に指定したとき, the Relation Deleter shall 確認を求めずに削除を実行する
4. While 対話的な入力ができない環境で、確認の省略が指定されていない間, the Relation Deleter shall 削除を実行せず、確認の省略が必要である旨を表示して0以外の終了コードで終了する
5. When 削除を実行するとき, the Relation Deleter shall 子孫レコードから起点レコードの順に削除し、削除計画のすべての削除を一つの不可分な単位として確定する
6. When 参照関係に自己参照や循環参照が含まれるとき, the Relation Deleter shall 削除計画のすべてのレコードを削除し終える
7. If 削除の途中でいずれかの削除が失敗したとき, the Relation Deleter shall それまでの削除をすべて取り消し、失敗したテーブルとエラー内容を表示して0以外の終了コードで終了する
8. When 削除が完了したとき, the Relation Deleter shall テーブルごとの実削除件数と総件数を表示し、終了コード0で終了する
9. If 実削除件数が削除計画の件数と一致しないとき, the Relation Deleter shall すべての削除を取り消し、差異のあるテーブルを表示して0以外の終了コードで終了する

### Requirement 7: 件数上限ガード
**Objective:** As a データベース運用者, I want 想定外に大量のレコードが削除対象になったら止めたい, so that 関連の連鎖による大規模な誤削除を防げる

#### Acceptance Criteria
1. Where 運用者が削除対象件数の上限を指定したとき, the Relation Deleter shall 削除対象の総件数が上限を超えた場合、削除を実行せずに、総件数と上限値を表示して0以外の終了コードで終了する
2. Where 運用者が削除対象件数の上限を指定しないとき, the Relation Deleter shall 件数による制限なく処理する

### Requirement 8: 大量データの処理
**Objective:** As a データベース運用者, I want 削除対象が大量でも処理を完了させたい, so that 大規模なデータでもツールを使える

#### Acceptance Criteria
1. When 削除対象が1テーブルあたり数万件を超えるとき, the Relation Deleter shall データベースの1文あたりのサイズ上限によるエラーを起こさずに、特定・プレビュー・削除を完了する
2. While 大量の削除対象を処理している間, the Relation Deleter shall 処理中のテーブルと進捗件数を表示する

### Requirement 9: 認証情報と接続の扱い
**Objective:** As a データベース運用者, I want パスワードを安全に渡したい, so that 認証情報が他のユーザーやログに漏れない

#### Acceptance Criteria
1. The Schema Dump Script and the Relation Deleter shall パスワードをコマンドライン引数で受け取らずに、環境変数または接続設定ファイルで受け取れるようにする
2. The Schema Dump Script and the Relation Deleter shall パスワードを画面出力やエラーメッセージに含めない
3. If データベースへの接続に失敗したとき, the Relation Deleter shall 接続先（ホスト・ポート・データベース名）とエラー内容を表示し、0以外の終了コードで終了する
