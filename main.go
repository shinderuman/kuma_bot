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
	"regexp"
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
	KumaNewsURL            = "https://topics.smt.docomo.ne.jp/latestnews/keywords/592c8cd81446273da9280cdf06875ec2347a5b3bd970bca305d5cb869e7c4161"
	MaxPages               = 3
	PostedURLRetentionDays = 30
	TootFetchLimit         = 40
	JSTOffset              = 9 * 60 * 60
	PostDelay              = 200 * time.Millisecond
	HTTPTimeout            = 30 * time.Second
	KumaPostTemplate       = `🐻 %s

🔗 %s

📍 %s

#クマ出没情報`
	SummaryPostTemplate = `🐻 %sのクマ出没情報集計（全%d件）
※あくまで出没情報記事数の集計なので実際の出没数とは限りません

📍 都道府県別ランキング:
%s

#クマ出没情報`

	prefecturePattern = `📍\s*([^\n📍]+)`
)

var prefectures = []string{
	"北海道", "青森県", "岩手県", "宮城県", "秋田県", "山形県", "福島県",
	"茨城県", "栃木県", "群馬県", "埼玉県", "千葉県", "東京都", "神奈川県",
	"新潟県", "富山県", "石川県", "福井県", "山梨県", "長野県", "岐阜県",
	"静岡県", "愛知県", "三重県", "滋賀県", "京都府", "大阪府", "兵庫県",
	"奈良県", "和歌山県", "鳥取県", "島根県", "岡山県", "広島県", "山口県",
	"徳島県", "香川県", "愛媛県", "高知県", "福岡県", "佐賀県", "長崎県",
	"熊本県", "大分県", "宮崎県", "鹿児島県", "沖縄県",
}

type MastodonConfig struct {
	Server       string `json:"server"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	AccessToken  string `json:"access_token"`
	Visibility   string `json:"visibility"`
}

type S3Config struct {
	BucketName string `json:"bucket_name"`
	ObjectKey  string `json:"object_key"`
}

type AWSConfig struct {
	Region string   `json:"region"`
	S3     S3Config `json:"s3"`
}

type Config struct {
	Mastodon MastodonConfig `json:"mastodon"`
	AWS      AWSConfig      `json:"aws"`
}

type PostedURL struct {
	URL         string    `json:"url"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	PublishedAt time.Time `json:"published_at"`
	PostedAt    time.Time `json:"posted_at"`
}

type PrefectureCount struct {
	Prefecture string `json:"prefecture"`
	Count      int    `json:"count"`
}

func main() {
	if isLambda() {
		lambda.Start(handleKumaBotRequest)
	} else {
		if err := handleKumaBotRequest(context.Background()); err != nil {
			log.Fatal(err)
		}
	}
}

func isLambda() bool {
	return len(os.Getenv("AWS_LAMBDA_FUNCTION_NAME")) > 0
}

func handleKumaBotRequest(ctx context.Context) error {
	config, err := loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	client := newMastodonClient(config)

	if isMidnightJST() || os.Getenv("KUMA_FORCE_SUMMARY") != "" {
		log.Println("Starting prefecture summary mode")
		if err := runPrefectureSummary(ctx, config, client); err != nil {
			return fmt.Errorf("failed to run prefecture summary: %w", err)
		}
		log.Println("Completed prefecture summary mode")
	}

	log.Println("Starting normal mode - checking bear sightings")
	existingURLs, err := loadPostedURLs(ctx, config)
	if err != nil {
		return fmt.Errorf("failed to load posted URLs: %w", err)
	}

	existingURLs = cleanupOldURLs(existingURLs)

	newPostedURLs, err := processLatestNews(existingURLs)
	if err != nil {
		return fmt.Errorf("failed to process latest news: %w", err)
	}

	if len(newPostedURLs) > 0 {
		successfullyPostedURLs := postToMastodon(ctx, config, client, newPostedURLs)

		if err := savePostedURLs(ctx, config, append(existingURLs, successfullyPostedURLs...)); err != nil {
			return fmt.Errorf("failed to save posted URLs: %w", err)
		}
	}

	return nil
}

func loadConfig() (*Config, error) {
	if isLambda() {
		return &Config{
			Mastodon: MastodonConfig{
				Server:       os.Getenv("MASTODON_SERVER"),
				ClientID:     os.Getenv("MASTODON_CLIENT_ID"),
				ClientSecret: os.Getenv("MASTODON_CLIENT_SECRET"),
				AccessToken:  os.Getenv("MASTODON_ACCESS_TOKEN"),
				Visibility:   os.Getenv("MASTODON_VISIBILITY"),
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

	file, err := os.Open("config.json")
	if err != nil {
		return nil, fmt.Errorf("failed to open config.json: %w", err)
	}
	defer file.Close()

	var config Config
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("failed to decode config.json: %w", err)
	}

	return &config, nil
}

func getAWSRegion() string {
	if region := os.Getenv("KUMA_AWS_REGION"); region != "" {
		return region
	}
	if region := os.Getenv("AWS_REGION"); region != "" {
		return region
	}
	return "ap-northeast-1"
}

func newMastodonClient(config *Config) *mastodon.Client {
	return mastodon.NewClient(&mastodon.Config{
		Server:       config.Mastodon.Server,
		ClientID:     config.Mastodon.ClientID,
		ClientSecret: config.Mastodon.ClientSecret,
		AccessToken:  config.Mastodon.AccessToken,
	})
}

func isMidnightJST() bool {
	jst := time.FixedZone("JST", JSTOffset)
	now := time.Now().In(jst)

	return now.Hour() == 0 && now.Minute() == 0
}

func runPrefectureSummary(ctx context.Context, config *Config, client *mastodon.Client) error {
	jst := time.FixedZone("JST", JSTOffset)
	yesterday := time.Now().In(jst).AddDate(0, 0, -1)

	toots, err := fetchRecentToots(ctx, client, yesterday)
	if err != nil {
		return fmt.Errorf("failed to fetch recent toots: %w", err)
	}

	prefectureStats := aggregatePrefectures(toots)

	if err := postPrefectureSummary(ctx, config, client, prefectureStats, len(toots), yesterday); err != nil {
		return fmt.Errorf("failed to post prefecture summary: %w", err)
	}

	return nil
}

func loadPostedURLs(ctx context.Context, appConfig *Config) ([]PostedURL, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(appConfig.AWS.Region))
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	svc := s3.NewFromConfig(cfg)

	result, err := svc.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(appConfig.AWS.S3.BucketName),
		Key:    aws.String(appConfig.AWS.S3.ObjectKey),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get object from S3: %w", err)
	}
	defer result.Body.Close()

	body, err := io.ReadAll(result.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read S3 object body: %w", err)
	}

	var postedURLs []PostedURL
	if err := json.Unmarshal(body, &postedURLs); err != nil {
		return nil, fmt.Errorf("failed to unmarshal posted URLs: %w", err)
	}

	return postedURLs, nil
}

func cleanupOldURLs(existingURLs []PostedURL) []PostedURL {
	cutoffTime := time.Now().AddDate(0, 0, -PostedURLRetentionDays)

	var validURLs []PostedURL
	for _, posted := range existingURLs {
		if posted.PostedAt.After(cutoffTime) {
			validURLs = append(validURLs, posted)
		}
	}

	return validURLs
}

func processLatestNews(existingURLs []PostedURL) ([]PostedURL, error) {
	existingURLMap := make(map[string]struct{})
	for _, posted := range existingURLs {
		existingURLMap[posted.URL] = struct{}{}
	}

	var allArticles []*PostedURL

	for page := 1; page <= MaxPages; page++ {
		doc, err := fetchHTML(page)
		if err != nil {
			if page == 1 {
				return nil, err
			}
			log.Printf("Failed to fetch page %d, stopping: %v", page, err)
			break
		}

		articles := parseArticles(doc, page)
		if len(articles) == 0 && page > 1 {
			log.Printf("No articles found on page %d, stopping", page)
			break
		}

		allArticles = append(allArticles, articles...)
	}

	var newPostedURLs []PostedURL
	for _, article := range allArticles {
		if _, exists := existingURLMap[article.URL]; !exists {
			newPostedURLs = append(newPostedURLs, *article)
		}
	}

	sort.Slice(newPostedURLs, func(i, j int) bool {
		return newPostedURLs[i].PublishedAt.Before(newPostedURLs[j].PublishedAt)
	})

	return newPostedURLs, nil
}

func postToMastodon(ctx context.Context, config *Config, client *mastodon.Client, articles []PostedURL) []PostedURL {
	var successfullyPosted []PostedURL
	for i, article := range articles {
		success := postSingleArticle(ctx, config, client, &article)
		if success {
			article.PostedAt = time.Now()
			successfullyPosted = append(successfullyPosted, article)
		}

		if i < len(articles)-1 {
			time.Sleep(PostDelay)
		}
	}

	return successfullyPosted
}

func savePostedURLs(ctx context.Context, appConfig *Config, postedURLs []PostedURL) error {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(appConfig.AWS.Region))
	if err != nil {
		return fmt.Errorf("failed to load AWS config: %w", err)
	}

	svc := s3.NewFromConfig(cfg)

	data, err := json.MarshalIndent(postedURLs, "", "    ")
	if err != nil {
		return fmt.Errorf("failed to marshal posted URLs: %w", err)
	}

	contentType := "application/json"
	_, err = svc.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(appConfig.AWS.S3.BucketName),
		Key:         aws.String(appConfig.AWS.S3.ObjectKey),
		Body:        bytes.NewReader(data),
		ContentType: &contentType,
	})
	if err != nil {
		return fmt.Errorf("failed to put object to S3: %w", err)
	}

	return nil
}

func fetchRecentToots(ctx context.Context, client *mastodon.Client, since time.Time) ([]*mastodon.Status, error) {
	account, err := client.GetAccountCurrentUser(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get current account: %w", err)
	}

	var allToots []*mastodon.Status
	maxID := ""

	for {
		var toots []*mastodon.Status

		if maxID != "" {
			toots, err = client.GetAccountStatuses(ctx, account.ID, &mastodon.Pagination{
				MaxID: mastodon.ID(maxID),
				Limit: TootFetchLimit,
			})
		} else {
			toots, err = client.GetAccountStatuses(ctx, account.ID, &mastodon.Pagination{
				Limit: TootFetchLimit,
			})
		}

		if err != nil {
			return nil, fmt.Errorf("failed to fetch account statuses: %w", err)
		}

		if len(toots) == 0 {
			break
		}

		for _, toot := range toots {
			if toot.CreatedAt.After(since) {
				allToots = append(allToots, toot)
			} else {
				return allToots, nil
			}
		}

		maxID = string(toots[len(toots)-1].ID)
	}

	return allToots, nil
}

func aggregatePrefectures(toots []*mastodon.Status) []PrefectureCount {
	prefectureCountMap := make(map[string]int)
	prefectureRegex := regexp.MustCompile(prefecturePattern)

	var otherCount int
	for _, toot := range toots {
		matches := prefectureRegex.FindStringSubmatch(toot.Content)
		if len(matches) > 1 {
			location := strings.TrimSpace(matches[1])

			prefecture := extractPrefecture(location)
			if prefecture != "" {
				prefectureCountMap[prefecture]++
			} else {
				otherCount++
			}
		}
	}

	var results []PrefectureCount
	for prefecture, count := range prefectureCountMap {
		results = append(results, PrefectureCount{
			Prefecture: prefecture,
			Count:      count,
		})
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].Count == results[j].Count {
			return results[i].Prefecture < results[j].Prefecture
		}
		return results[i].Count > results[j].Count
	})

	if otherCount > 0 {
		results = append(results, PrefectureCount{
			Prefecture: "その他",
			Count:      otherCount,
		})
	}

	return results
}

func postPrefectureSummary(ctx context.Context, config *Config, client *mastodon.Client, stats []PrefectureCount, totalPosts int, date time.Time) error {
	dateStr := date.Format("2006年1月2日")
	postContent := fmt.Sprintf(SummaryPostTemplate, dateStr, totalPosts, formatPrefectureStats(stats))

	status, err := postToMastodonWithContent(ctx, config, client, postContent)
	if err != nil {
		return fmt.Errorf("failed to post prefecture summary: %w", err)
	}

	if err := pinSummaryPosts(ctx, client, status.ID); err != nil {
		log.Printf("Failed to pin summary post: %v", err)
	}

	return nil
}

func fetchHTML(page int) (*goquery.Document, error) {
	client := &http.Client{Timeout: HTTPTimeout}
	url := fmt.Sprintf("%s?page=%d", KumaNewsURL, page)

	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch page %d from %s: %w", page, KumaNewsURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("page %d not found (404)", page)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP error %d when fetching page %d", resp.StatusCode, page)
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to parse HTML from page %d: %w", page, err)
	}

	return doc, nil
}

func parseArticles(doc *goquery.Document, page int) []*PostedURL {
	var articles []*PostedURL

	doc.Find("li.h-bm02").Each(func(i int, s *goquery.Selection) {
		if s.Find("div[data-allox-placement]").Length() > 0 {
			return
		}

		article := extractArticleInfo(s, page)
		if article != nil {
			articles = append(articles, article)
		}
	})

	return articles
}

func extractArticleInfo(s *goquery.Selection, page int) *PostedURL {
	thumbsListUnit := s.Find("div.thumbsListUnit")
	newsListSupplement := thumbsListUnit.Find("p.newsListSupplement")
	dateText := strings.TrimSpace(newsListSupplement.Find("span.newsDate").Text())
	timeText := strings.TrimSpace(newsListSupplement.Find("span.newsTime").Text())

	timestamp, err := parseDateTime(dateText, timeText)
	if err != nil {
		log.Printf("Skipping article on page %d due to datetime parse error: %v", page, err)
		return nil
	}

	title := strings.TrimSpace(thumbsListUnit.Find("h3.thumbsListTitle").Text())
	href, _ := thumbsListUnit.Find("h3.thumbsListTitle").Closest("a").Attr("href")
	source := strings.TrimSpace(newsListSupplement.Find("span.newsTenter").Text())
	region := strings.TrimSpace(s.Find("ul.topics-keywords li a").Text())

	return &PostedURL{
		Title:       title,
		URL:         href,
		Description: fmt.Sprintf("%s %s %s %s", region, source, dateText, timeText),
		PublishedAt: timestamp,
	}
}

func postSingleArticle(ctx context.Context, config *Config, client *mastodon.Client, article *PostedURL) bool {
	post := fmt.Sprintf(KumaPostTemplate, article.Title, article.URL, article.Description)

	_, err := postToMastodonWithContent(ctx, config, client, post)
	if err != nil {
		log.Printf("Failed to post article '%s': %v", article.Title, err)
		return false
	}

	return true
}

func postToMastodonWithContent(ctx context.Context, config *Config, client *mastodon.Client, content string) (*mastodon.Status, error) {
	status, err := client.PostStatus(ctx, &mastodon.Toot{
		Status:     content,
		Visibility: config.Mastodon.Visibility,
	})
	if err != nil {
		log.Printf("Failed to post content to Mastodon:\n%s\nError: %v", content, err)
		return nil, fmt.Errorf("failed to post to Mastodon: %w", err)
	}

	return status, nil
}

func pinSummaryPosts(ctx context.Context, client *mastodon.Client, newStatusID mastodon.ID) error {
	account, err := client.GetAccountCurrentUser(ctx)
	if err != nil {
		return fmt.Errorf("failed to get current account: %w", err)
	}

	pinnedStatuses, err := client.GetAccountPinnedStatuses(ctx, account.ID)
	if err != nil {
		return fmt.Errorf("failed to get pinned statuses: %w", err)
	}

	if len(pinnedStatuses) >= 5 {
		sort.Slice(pinnedStatuses, func(i, j int) bool {
			return pinnedStatuses[i].CreatedAt.Before(pinnedStatuses[j].CreatedAt)
		})

		oldestPinned := pinnedStatuses[0]
		req, err := http.NewRequestWithContext(ctx, "POST",
			fmt.Sprintf("%s/api/v1/statuses/%s/unpin", client.Config.Server, oldestPinned.ID), nil)
		if err != nil {
			return fmt.Errorf("failed to create unpin request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+client.Config.AccessToken)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("failed to unpin oldest status: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("failed to unpin status, got status code: %d", resp.StatusCode)
		}
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/api/v1/statuses/%s/pin", client.Config.Server, newStatusID), nil)
	if err != nil {
		return fmt.Errorf("failed to create pin request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+client.Config.AccessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to pin new summary status: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to pin status, got status code: %d", resp.StatusCode)
	}

	return nil
}

func extractPrefecture(text string) string {
	for _, prefecture := range prefectures {
		if strings.Contains(text, prefecture) {
			return prefecture
		}
	}

	return ""
}

func formatPrefectureStats(stats []PrefectureCount) string {
	var lines []string
	for i, stat := range stats {
		lines = append(lines, fmt.Sprintf("%d. %s：%d件", i+1, stat.Prefecture, stat.Count))
	}
	return strings.Join(lines, "\n")
}

func parseDateTime(dateText, timeText string) (time.Time, error) {
	jst := time.FixedZone("JST", JSTOffset)
	nowJST := time.Now().In(jst)

	if idx := strings.Index(dateText, "("); idx > 0 {
		dateText = dateText[:idx]
	}

	dateTimeStr := fmt.Sprintf("%d/%s %s", nowJST.Year(), dateText, timeText)

	parsedTime, err := time.ParseInLocation("2006/1/2 15:4", dateTimeStr, jst)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to parse datetime '%s %s': %v", dateText, timeText, err)
	}

	if parsedTime.After(nowJST) {
		dateTimeStr = fmt.Sprintf("%d/%s %s", nowJST.Year()-1, dateText, timeText)
		parsedTime, err = time.ParseInLocation("2006/1/2 15:4", dateTimeStr, jst)
		if err != nil {
			return time.Time{}, fmt.Errorf("failed to parse previous year datetime '%s': %v", dateTimeStr, err)
		}
	}

	return parsedTime, nil
}
