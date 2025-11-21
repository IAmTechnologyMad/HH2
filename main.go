package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	API_URL_PAGE_1 = "https://www.firstcry.com/svcs/Search"
	API_URL_PAGE_2 = "https://www.firstcry.com/svcs/Search"
	TELEGRAM_API   = "https://api.telegram.org/bot"
)

var (
	checkInterval   = 5 * time.Second
	lastCheck       = time.Now()
	productsSeen    = make(map[string]Product)
	mutex           sync.Mutex
	telegramToken   = os.Getenv("TELEGRAM_TOKEN")
	TELEGRAM_CHAT_ID = os.Getenv("TELEGRAM_CHAT_ID")
	heartbeatMuted  = true
)

type Product struct {
	ID    string
	Title string
	Image string
	Price float64
	Link  string
}

type SearchResponse struct {
	SearchResult struct {
		Data struct {
			ProductList []struct {
				ProductID   string  `json:"ProductId"`
				ProductName string  `json:"ProductName"`
				Price       float64 `json:"Price"`
				ProductImg  string  `json:"Image"`
			} `json:"ProductList"`
		} `json:"data"`
	} `json:"SearchResult"`
}

type CheckResult struct {
	Timestamp     time.Time
	FoundProducts []Product
}

var checkHistory []CheckResult

func sendTelegramMessage(chatID string, msg string) {
	if telegramToken == "" {
		log.Println("⚠️ TELEGRAM_TOKEN not set")
		return
	}

	resp, err := http.PostForm(
		TELEGRAM_API+telegramToken+"/sendMessage",
		url.Values{
			"chat_id": {chatID},
			"text":    {msg},
		},
	)
	if err != nil {
		log.Println("Telegram error:", err)
		return
	}
	defer resp.Body.Close()
}

func fetchPage(apiURL string, page int) ([]Product, error) {
	client := &http.Client{Timeout: 10 * time.Second}

	payload := fmt.Sprintf("pgno=%d&pagesize=200", page)
	req, err := http.NewRequest("POST", apiURL, strings.NewReader(payload))
	if err != nil {
		return nil, err
	}

	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)

	var res SearchResponse
	err = json.Unmarshal(bodyBytes, &res)
	if err != nil {
		return nil, err
	}

	products := []Product{}
	for _, p := range res.SearchResult.Data.ProductList {
		products = append(products, Product{
			ID:    p.ProductID,
			Title: p.ProductName,
			Price: p.Price,
			Image: p.ProductImg,
			Link:  "https://www.firstcry.com/" + p.ProductID,
		})
	}

	return products, nil
}

func scrapeAllPages() ([]Product, error) {
	all := []Product{}

	first, err := fetchPage(API_URL_PAGE_1, 1)
	if err != nil {
		return nil, err
	}
	all = append(all, first...)

	second, err := fetchPage(API_URL_PAGE_2, 2)
	if err == nil {
		all = append(all, second...)
	}

	return all, nil
}

func scraperWorker() {
	for {
		time.Sleep(checkInterval)

		mutex.Lock()
		lastCheck = time.Now()
		mutex.Unlock()

		products, err := scrapeAllPages()
		if err != nil {
			log.Println("Scrape error:", err)
			continue
		}

		newlyFound := []Product{}

		mutex.Lock()
		for _, p := range products {
			if _, ok := productsSeen[p.ID]; !ok {
				productsSeen[p.ID] = p
				newlyFound = append(newlyFound, p)
			}
		}
		isMuted := heartbeatMuted

		checkHistory = append(checkHistory, CheckResult{
			Timestamp:     time.Now(),
			FoundProducts: newlyFound,
		})
		if len(checkHistory) > 10 {
			checkHistory = checkHistory[1:]
		}

		mutex.Unlock()

		if len(newlyFound) == 0 {
			log.Println("...No new items found.")
			continue
		}

		if !isMuted {
			// No heartbeat message
		}

		for _, p := range newlyFound {
			msg := fmt.Sprintf(
				"🔥 *New Hot Wheels Found!*\n\nName: %s\nPrice: ₹%.0f\nLink: %s",
				p.Title, p.Price, p.Link,
			)
			sendTelegramMessage(TELEGRAM_CHAT_ID, msg)
		}
	}
}

func apiHandler(w http.ResponseWriter, r *http.Request) {
	mutex.Lock()
	defer mutex.Unlock()

	type APIResponse struct {
		Running        bool          `json:"running"`
		LastCheck      string        `json:"last_check"`
		ProductsSeen   int           `json:"products_seen"`
		HeartbeatMuted bool          `json:"heartbeat_muted"`
		CheckHistory   []CheckResult `json:"checks"`
	}

	json.NewEncoder(w).Encode(APIResponse{
		Running:        true,
		LastCheck:      lastCheck.Format(time.RFC3339),
		ProductsSeen:   len(productsSeen),
		HeartbeatMuted: heartbeatMuted,
		CheckHistory:   checkHistory,
	})
}

func main() {
	go scraperWorker()

	http.HandleFunc("/", apiHandler)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Println("Server running on port:", port)
	http.ListenAndServe(":"+port, nil)
}
