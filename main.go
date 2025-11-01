package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/PuerkitoBio/goquery"
	"github.com/mattn/go-mastodon"
)

const (
	// kumaNewsURL クマ出没情報のニュースURL
	kumaNewsURL = "https://topics.smt.docomo.ne.jp/latestnews/keywords/592c8cd81446273da9280cdf06875ec2347a5b3bd970bca305d5cb869e7c4161"

	// MaxPages 取得する最大ページ数
	MaxPages = 3

	// PostedURLRetentionDays 投稿済みURL保持日数
	PostedURLRetentionDays = 30
)

// MastodonConfig Mastodon設定
type MastodonConfig struct {
	Server       string `json:"server"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	AccessToken  string `json:"access_token"`
}

// S3Config S3設定
type S3Config struct {
	BucketName string `json:"bucket_name"`
	ObjectKey  string `json:"object_key"`
}

// AWSConfig AWS設定
type AWSConfig struct {
	Region string   `json:"region"`
	S3     S3Config `json:"s3"`
}

// Config アプリケーション設定
type Config struct {
	Mastodon MastodonConfig `json:"mastodon"`
	AWS      AWSConfig      `json:"aws"`
}

// PostedURL 投稿済みURL情報の構造体
type PostedURL struct {
	URL         string    `json:"url"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	PublishedAt time.Time `json:"published_at"`
	PostedAt    time.Time `json:"posted_at"`
}

// main メイン関数 - Lambda環境とローカル環境を判定
func main() {
	// Lambda環境かどうかを判定
	if isLambda() {
		// Lambda環境ではハンドラーを起動
		lambda.Start(runKumaBot)
	} else {
		// ローカル環境では直接実行
		if err := runKumaBot(context.Background()); err != nil {
			log.Fatal(err)
		}
	}
}

// isLambda Lambda環境かどうかを判定
func isLambda() bool {
	return len(os.Getenv("AWS_LAMBDA_FUNCTION_NAME")) > 0
}

// runKumaBot クマbotのメイン処理 - Lambdaハンドラーとしても使用
func runKumaBot(ctx context.Context) error {
	log.Println("Kuma Bot started - クマ出没情報をチェックします")

	// 設定を読み込み
	config, err := loadConfig()
	if err != nil {
		log.Printf("Failed to load config: %v", err)
		return err
	}

	// 投稿済みURLを読み込み
	postedURLs, err := loadPostedURLs(ctx, config)
	if err != nil {
		log.Printf("Failed to load posted URLs: %v", err)
		return err
	}

	// 古いURLを削除
	postedURLs = cleanupOldURLs(postedURLs)

	newPostedURLs, err := processLatestNews(postedURLs)
	if err != nil {
		return err
	}

	// Mastodonに投稿
	successfullyPostedURLs := postToMastodon(ctx, config, newPostedURLs)

	// 投稿済みURLを保存
	return savePostedURLs(ctx, config, append(postedURLs, successfullyPostedURLs...))
}

// loadConfig 設定を読み込み
func loadConfig() (*Config, error) {
	// Lambda環境では環境変数から取得
	if isLambda() {
		return &Config{
			Mastodon: MastodonConfig{
				Server:       os.Getenv("MASTODON_SERVER"),
				ClientID:     os.Getenv("MASTODON_CLIENT_ID"),
				ClientSecret: os.Getenv("MASTODON_CLIENT_SECRET"),
				AccessToken:  os.Getenv("MASTODON_ACCESS_TOKEN"),
			},
			AWS: AWSConfig{
				Region: getAWSRegion(),
				S3: S3Config{
					BucketName: os.Getenv("S3_BUCKET_NAME"),
					ObjectKey:  os.Getenv("S3_OBJECT_KEY"),
				},
			},
		}, nil
	}

	// ローカル環境ではconfig.jsonから取得
	file, err := os.Open("config.json")
	if err != nil {
		return nil, fmt.Errorf("failed to open config.json: %v", err)
	}
	defer file.Close()

	var config Config
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("failed to decode config.json: %v", err)
	}

	return &config, nil
}

// getAWSRegion AWSリージョンを取得（カスタム環境変数を優先）
func getAWSRegion() string {
	// カスタム環境変数を優先
	if region := os.Getenv("KUMA_AWS_REGION"); region != "" {
		return region
	}
	// Lambda予約済み環境変数をフォールバック
	if region := os.Getenv("AWS_REGION"); region != "" {
		return region
	}
	// デフォルト値
	return "ap-northeast-1"
}

// loadPostedURLs S3から投稿済みURLを読み込み
func loadPostedURLs(ctx context.Context, appConfig *Config) ([]PostedURL, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(appConfig.AWS.Region))
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %v", err)
	}

	svc := s3.NewFromConfig(cfg)

	result, err := svc.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(appConfig.AWS.S3.BucketName),
		Key:    aws.String(appConfig.AWS.S3.ObjectKey),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get object from S3: %v", err)
	}
	defer result.Body.Close()

	body, err := io.ReadAll(result.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read S3 object body: %v", err)
	}

	var postedURLs []PostedURL
	if err := json.Unmarshal(body, &postedURLs); err != nil {
		return nil, fmt.Errorf("failed to unmarshal posted URLs: %v", err)
	}

	log.Printf("Loaded %d posted URLs from S3", len(postedURLs))
	return postedURLs, nil
}

// cleanupOldURLs 30日以上経過した投稿済みURLを削除
func cleanupOldURLs(postedURLs []PostedURL) []PostedURL {
	cutoffTime := time.Now().AddDate(0, 0, -PostedURLRetentionDays)

	var validURLs []PostedURL
	for _, posted := range postedURLs {
		if posted.PostedAt.After(cutoffTime) {
			validURLs = append(validURLs, posted)
		}
	}

	return validURLs
}

// processLatestNews 最新のクマ出没ニュースを処理
func processLatestNews(postedURLs []PostedURL) ([]PostedURL, error) {
	// 投稿済みURLのマップを作成
	postedURLMap := make(map[string]struct{})
	for _, posted := range postedURLs {
		postedURLMap[posted.URL] = struct{}{}
	}

	var allKumaInfos []*PostedURL

	// 複数ページを取得
	for page := 1; page <= MaxPages; page++ {
		doc, err := fetchHTML(page)
		if err != nil {
			if page == 1 {
				return nil, err
			}
			log.Printf("Failed to fetch page %d, stopping: %v", page, err)
			break
		}

		kumaInfos := parseArticles(doc, page)
		if len(kumaInfos) == 0 && page > 1 {
			log.Printf("No articles found on page %d, stopping", page)
			break
		}

		allKumaInfos = append(allKumaInfos, kumaInfos...)
	}

	// 投稿済みでない記事のみをフィルタリング
	var newPostedURLs []PostedURL
	for _, info := range allKumaInfos {
		if _, exists := postedURLMap[info.URL]; !exists {
			newPostedURLs = append(newPostedURLs, *info)
		}
	}

	// 古い順でソート（PublishedAtを使用）
	sort.Slice(newPostedURLs, func(i, j int) bool {
		return newPostedURLs[i].PublishedAt.Before(newPostedURLs[j].PublishedAt)
	})

	log.Printf("Found %d new kuma news items (total %d, already posted %d)",
		len(newPostedURLs), len(allKumaInfos), len(allKumaInfos)-len(newPostedURLs))

	return newPostedURLs, nil
}

// fetchHTML クマニュースのHTMLを取得
func fetchHTML(page int) (*goquery.Document, error) {
	httpClient := &http.Client{Timeout: 30 * time.Second}

	resp, err := httpClient.Get(fmt.Sprintf("%s?page=%d", kumaNewsURL, page))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("page not found")
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP error: %d", resp.StatusCode)
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, err
	}

	return doc, nil
}

// parseArticles HTMLから記事を解析
func parseArticles(doc *goquery.Document, page int) []*PostedURL {
	var kumaInfos []*PostedURL
	var totalArticles int

	doc.Find("li.h-bm02").Each(func(i int, s *goquery.Selection) {
		// 広告要素をスキップ
		if s.Find("div[data-allox-placement]").Length() > 0 {
			return
		}

		totalArticles++

		// 記事情報を取得
		thumbsListUnit := s.Find("div.thumbsListUnit")
		newsListSupplement := thumbsListUnit.Find("p.newsListSupplement")
		dateText := strings.TrimSpace(newsListSupplement.Find("span.newsDate").Text())
		timeText := strings.TrimSpace(newsListSupplement.Find("span.newsTime").Text())

		timestamp, err := parseDateTime(dateText, timeText)
		if err != nil {
			log.Printf("Skipping article on page %d due to datetime parse error: %v", page, err)
			return
		}

		title := strings.TrimSpace(thumbsListUnit.Find("h3.thumbsListTitle").Text())
		href, _ := thumbsListUnit.Find("h3.thumbsListTitle").Closest("a").Attr("href")
		source := strings.TrimSpace(newsListSupplement.Find("span.newsTenter").Text())
		region := strings.TrimSpace(s.Find("ul.topics-keywords li a").Text())

		kumaInfos = append(kumaInfos, &PostedURL{
			Title:       title,
			URL:         href,
			Description: fmt.Sprintf("%s %s %s %s", region, source, dateText, timeText),
			PublishedAt: timestamp,
		})
	})

	return kumaInfos
}

// parseDateTime 日付と時刻文字列をtime.Timeに変換
func parseDateTime(dateText, timeText string) (time.Time, error) {
	// 日本時間のタイムゾーンを設定
	jst := time.FixedZone("JST", 9*60*60)
	nowJST := time.Now().In(jst)

	// 日付から曜日部分を除去 (例: "10/31(金)" -> "10/31")
	if idx := strings.Index(dateText, "("); idx > 0 {
		dateText = dateText[:idx]
	}

	// 現在の年を使って日時文字列を作成
	dateTimeStr := fmt.Sprintf("%d/%s %s", nowJST.Year(), dateText, timeText)

	// 日本時間として解析
	parsedTime, err := time.ParseInLocation("2006/1/2 15:4", dateTimeStr, jst)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to parse datetime '%s %s': %v", dateText, timeText, err)
	}

	// 年跨ぎ問題の対処: パースした日付が未来になる場合は前年の日付とする
	if parsedTime.After(nowJST) {
		dateTimeStr = fmt.Sprintf("%d/%s %s", nowJST.Year()-1, dateText, timeText)
		parsedTime, err = time.ParseInLocation("2006/1/2 15:4", dateTimeStr, jst)
		if err != nil {
			return time.Time{}, fmt.Errorf("failed to parse previous year datetime '%s': %v", dateTimeStr, err)
		}
	}

	return parsedTime, nil
}

// postToMastodon PostedURLをMastodonに投稿し、成功したURLを返す
func postToMastodon(ctx context.Context, config *Config, postedURLs []PostedURL) []PostedURL {
	// Mastodon設定を作成
	mastodonConfig := &mastodon.Config{
		Server:       config.Mastodon.Server,
		ClientID:     config.Mastodon.ClientID,
		ClientSecret: config.Mastodon.ClientSecret,
		AccessToken:  config.Mastodon.AccessToken,
	}

	// Mastodonクライアントを作成
	client := mastodon.NewClient(mastodonConfig)

	var successfullyPosted []PostedURL
	for i, posted := range postedURLs {
		// 投稿テキストを生成
		post := fmt.Sprintf(`🐻 %s

🔗 %s

📍 %s

#クマ出没情報`, posted.Title, posted.URL, posted.Description)

		_, err := client.PostStatus(ctx, &mastodon.Toot{
			Status:     post,
			Visibility: "unlisted",
		})
		if err != nil {
			log.Printf("Failed to post: %s - %v", posted.Title, err)
		} else {
			// 投稿成功時に投稿時刻を設定
			posted.PostedAt = time.Now()
			successfullyPosted = append(successfullyPosted, posted)
		}

		// 最後の投稿以外は0.2秒待機
		if i < len(postedURLs)-1 {
			time.Sleep(200 * time.Millisecond)
		}
	}

	log.Printf("Successfully posted %d out of %d posts", len(successfullyPosted), len(postedURLs))
	return successfullyPosted
}

// savePostedURLs 投稿済みURLをS3に保存
func savePostedURLs(ctx context.Context, appConfig *Config, postedURLs []PostedURL) error {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(appConfig.AWS.Region))
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %v", err)
	}

	svc := s3.NewFromConfig(cfg)

	data, err := json.MarshalIndent(postedURLs, "", "    ")
	if err != nil {
		return fmt.Errorf("failed to marshal posted URLs: %v", err)
	}

	contentType := "application/json"
	_, err = svc.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(appConfig.AWS.S3.BucketName),
		Key:         aws.String(appConfig.AWS.S3.ObjectKey),
		Body:        bytes.NewReader(data),
		ContentType: &contentType,
	})
	if err != nil {
		return fmt.Errorf("failed to put object to S3: %v", err)
	}

	log.Printf("Saved %d posted URLs to S3", len(postedURLs))
	return nil
}
