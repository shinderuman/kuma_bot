# Kuma Bot

クマ出没情報を自動収集してMastodonに投稿するGoアプリケーションです。AWS Lambdaでの実行とローカル実行の両方に対応しています。

## 機能

- クマ出没情報を自動収集（docomoニュース）
- **RSSニュースフィードからクマ関連ニュースを自動収集（NHK、Yahooニュース、朝日新聞など25+ソース）**
- 投稿済みURLをS3で管理し、重複投稿を防止
- 古い投稿記録の自動クリーンアップ（30日間保持）
- Mastodonへの自動投稿（unlisted設定）
- Lambda環境とローカル環境の自動判定
- **毎日0時（JST）に24時間分のクマ出没情報を都道府県別に集計して投稿**
- **クマ関連コンテンツのフィルタリング機能（包含/除外キーワード設定）**
- **Mastodon投稿の500文字制限対応**
- **DRY_RUNモード対応（テスト実行用）**

## セットアップ

### 1. 依存関係のインストール

```bash
go mod tidy
```

### 2. 設定ファイルの作成

```bash
cp config.json.example config.json
```

`config.json`を編集してMastodonの認証情報とAWS設定を設定：

```json
{
    "mastodon": {
        "server": "https://your-mastodon-server.com",
        "client_id": "your_client_id_here",
        "client_secret": "your_client_secret_here",
        "access_token": "your_access_token_here"
    },
    "aws": {
        "region": "ap-northeast-1",
        "s3": {
            "bucket_name": "kuma-posted-urls",
            "object_key": "posted_urls.json"
        }
    }
}
```

### 3. AWS設定（Lambda使用時）

設定ファイルで指定したS3バケットを作成し、適切なIAMロールを設定してください。

#### 必要なIAM権限
Lambda実行ロールには以下の権限が必要です：
- S3バケットへの読み書き権限（GetObject, PutObject）
- CloudWatch Logsへの書き込み権限

## 使用方法

### ローカル実行

```bash
# 通常モード実行
go run main.go

# 集計モードを実行（強制的に集計と通常モード両方実行）
KUMA_FORCE_SUMMARY=1 go run main.go

# ドライランモード（投稿やS3更新を行わずテスト）
DRY_RUN=1 go run main.go

# ドライランモードで集計をテスト
DRY_RUN=1 KUMA_FORCE_SUMMARY=1 go run main.go
```

### Lambda デプロイ

```bash
# デフォルト設定でデプロイ
./deploy.sh

# カスタムプロファイルでデプロイ
./deploy.sh my-profile

# カスタムプロファイルと関数名でデプロイ
./deploy.sh my-profile my-function-name
```

## 環境変数

### Lambda環境
Lambda環境では以下の環境変数を設定してください：

- `MASTODON_SERVER` - MastodonサーバーのURL
- `MASTODON_CLIENT_ID` - MastodonアプリのクライアントID
- `MASTODON_CLIENT_SECRET` - Mastodonアプリのクライアントシークレット
- `MASTODON_ACCESS_TOKEN` - Mastodonのアクセストークン
- `MASTODON_VISIBILITY` - 投稿の可視性（オプション、デフォルト: unlisted）
- `S3_BUCKET_NAME` - S3バケット名
- `S3_OBJECT_KEY` - S3オブジェクトキー
- `KUMA_AWS_REGION` - AWSリージョン（オプション、`AWS_REGION`が優先される）

**注意**: `KUMA_AWS_REGION`を設定することで、Lambda環境でもカスタムリージョンを指定できます。設定しない場合は`AWS_REGION`（Lambda予約済み環境変数）が使用されます。

### ローカル実行（オプション）
ローカルでテスト用に使用できる環境変数：

- `KUMA_FORCE_SUMMARY` - 集計モードを強制実行（空以外の値で有効）
- `DRY_RUN` - ドライランモード（投稿やS3更新を行わず、ログのみ出力）

## 設定

### 定数設定

`main.go`内の定数で動作をカスタマイズできます：

- `MaxPages` - 取得する最大ページ数（デフォルト: 3）
- `PostedURLRetentionDays` - 投稿済みURL保持日数（デフォルト: 30日）

### 設定ファイル項目

#### `mastodon` - Mastodon接続設定
- `server` - MastodonサーバーのURL
- `client_id` - アプリケーションのクライアントID
- `client_secret` - アプリケーションのクライアントシークレット
- `access_token` - ユーザーのアクセストークン

#### `aws` - AWS設定
- `region` - AWSリージョン（例: ap-northeast-1）
- `s3.bucket_name` - 投稿済みURL管理用S3バケット名
- `s3.object_key` - S3オブジェクトキー（JSONファイル名）

### 投稿形式

#### クマ出没情報投稿
```
🐻 [記事タイトル]

🔗 [記事URL]

📍 [地域] [情報源] [日付] [時刻]

#クマ出没情報
```

#### RSSニュース投稿
```
📰 クマ関連ニュース：[記事タイトル]

[記事URL]

🔗 [記事概要]

#クマ関連ニュース
```

#### 都道府県別集計投稿（毎日0時JST）
```
🐻 2025年1月2日のクマ出没情報集計（全〇件）
※あくまで出没情報記事数の集計なので実際の出没数とは限りません

📍 都道府県別ランキング:
 1. 秋田県：〇件
 2. 福島県：〇件
 3. 岩手県：〇件
...
    その他：〇件

#クマ出没情報
```

**機能説明：**
- 同件数の場合は同じ順位を表示（例：2位が2件あれば、次は4位）
- 「その他」はランキング対象外として末尾に表示
- 集計データは過去24時間分の投稿を対象

## ファイル構成

```
.
├── main.go              # メインアプリケーション
├── config.json          # 設定ファイル（Git管理対象外）
├── config.json.example  # 設定ファイルのサンプル
├── deploy.sh            # Lambdaデプロイスクリプト
├── go.mod               # Go モジュール定義
├── go.sum               # Go モジュール依存関係
├── .gitignore           # Git除外設定
└── README.md            # このファイル
```

## 依存ライブラリ

- `github.com/aws/aws-lambda-go/lambda` - AWS Lambda Go SDK
- `github.com/aws/aws-sdk-go-v2/*` - AWS SDK for Go v2
- `github.com/PuerkitoBio/goquery` - HTMLパースとスクレイピング
- `github.com/mattn/go-mastodon` - Mastodon API クライアント
- `github.com/mmcdole/gofeed` - RSSフィードパーサー

## 技術仕様

### 環境判定
- `AWS_LAMBDA_FUNCTION_NAME`環境変数の存在でLambda環境を自動判定
- Lambda環境では環境変数、ローカル環境では`config.json`から設定を読み込み

### AWSリージョン設定
- `KUMA_AWS_REGION`環境変数を優先使用
- 設定されていない場合は`AWS_REGION`（Lambda予約済み環境変数）を使用
- ローカル環境では`config.json`の設定を使用

### 投稿制御
- 投稿間隔: 200ミリ秒
- 投稿可視性: unlisted
- 重複投稿防止: S3でURL管理
- データ保持期間: 30日間
- 集計実行: 毎日0時（JST）、Lambda環境で自動実行
- 集計対象: 過去24時間の投稿を都道府県別に集計
- RSS投稿: 500文字制限対応（文字数超過時は概要を省略）
- コンテンツフィルタリング: クマ関連キーワード判定と除外キーワード設定

### RSSフィード
- 25+の主要な日本のニュースソースを監視
- NHK、Yahooニュース、朝日新聞、毎日新聞、日本経済新聞など
- クマ関連ニュースの自動検出と投稿
- 重複URLの自動排除（RSSフィード間の重複も対応）

## 注意事項

- `config.json`は機密情報を含むため、Git管理に含めないでください
- S3バケットへの適切なアクセス権限が必要です
- Lambda環境では`AWS_REGION`環境変数が自動設定されます

## トラブルシューティング

### よくある問題

#### Lambda環境で`AWS_REGION`エラーが発生する
- `AWS_REGION`はLambdaの予約済み環境変数のため、手動設定できません
- カスタムリージョンが必要な場合は`KUMA_AWS_REGION`を使用してください

#### S3アクセスエラー
- Lambda実行ロールにS3バケットへの適切な権限があることを確認してください
- バケット名とオブジェクトキーが正しく設定されていることを確認してください

#### Mastodon投稿エラー
- アクセストークンが有効であることを確認してください
- サーバーURLが正しいことを確認してください

## ライセンス

このプロジェクトはMITライセンスの下で公開されています。