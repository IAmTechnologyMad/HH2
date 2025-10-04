package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// --- CONFIGURATION ---
const (
	// THE SPECIAL API URL FOR PAGE 1
	API_URL_PAGE_1 = "https://www.firstcry.com/svcs/SearchResult.svc/GetSearchResultProductsFilters?PageNo=1&PageSize=100&SortExpression=NewArrivals&OnSale=5&SearchString=brand&SubCatId=&BrandId=&Price=&Age=&Color=&OptionalFilter=&OutOfStock=&Type1=&Type2=&Type3=&Type4=&Type5=&Type6=&Type7=&Type8=&Type9=&Type10=&Type11=&Type12=&Type13=&Type14=&Type15=&combo=&discount=&searchwithincat=&ProductidQstr=&searchrank=&pmonths=&cgen=&PriceQstr=&DiscountQstr=&MasterBrand=113&sorting=&Rating=&Offer=&skills=&material=&curatedcollections=&measurement=&gender=&exclude=&premium=&pcode=680566&isclub=0&deliverytype="
	// THE REGULAR API URL TEMPLATE FOR PAGES 2 AND BEYOND
	API_URL_PAGING_TEMPLATE = "https://www.firstcry.com/svcs/SearchResult.svc/GetSearchResultProductsPaging?PageNo=%d&PageSize=20&SortExpression=NewArrivals&OnSale=5&SearchString=brand&SubCatId=&BrandId=&Price=&Age=&Color=&OptionalFilter=&OutOfStock=&Type1=&Type2=&Type3=&Type4=&Type5=&Type6=&Type7=&Type8=&Type9=&Type10=&Type11=&Type12=&Type13=&Type14=&Type15=&combo=&discount=&searchwithincat=&ProductidQstr=&searchrank=&pmonths=&cgen=&PriceQstr=&DiscountQstr=&sorting=&MasterBrand=113&Rating=&Offer=&skills=&material=&curatedcollections=&measurement=&gender=&exclude=&premium=&pcode=680566&isclub=0&deliverytype="
	PAGES_TO_SCAN           = 6 // Total pages to check (1 initial + 5 paging)
	TELEGRAM_BOT_TOKEN      = "8222224289:AAFDgJ2C0KSTks9lLhPKtUtR1KzqraNkybI"
	TELEGRAM_CHAT_ID        = "-4985438208"
	ADMIN_CHAT_ID           = "837428747"
	SEEN_ITEMS_FILE         = "seen_hotwheels_go.txt"
)

// --- SHARED STATE & DATA STRUCTS ---
var (
	mutex          sync.Mutex
	checkInterval  = 5 * time.Second  // 🔥 OPTIMIZED TO 5 SECONDS
	isPaused       = false
	heartbeatMuted = false
	seenItems      = make(map[string]bool) // The KEY will now be the ProductInfoID
	checkHistory   []CheckResult
)

// --- TELEGRAM STRUCTS ---
type TelegramUpdateResponse struct {
	Ok     bool     `json:"ok"`
	Result []Update `json:"result"`
}
type Update struct {
	UpdateID int     `json:"update_id"`
	Message  Message `json:"message"`
}
type Message struct {
	Text string `json:"text"`
	Chat Chat   `json:"chat"`
}
type Chat struct {
	ID int64 `json:"id"`
}

// --- API STRUCTS ---
type OuterEnvelope struct {
	ProductResponse string `json:"ProductResponse"`
}
type InnerData struct {
	Products []Product `json:"Products"`
}
type Product struct {
	ProductID     string `json:"PId"`
	ProductInfoID string `json:"PInfId"` // THE TRUE UNIQUE ID
	ProductName   string `json:"PNm"`
	Price         string `json:"discprice"`
	StockStatus   string `json:"CrntStock"`
}

// --- HISTORY STRUCT ---
type CheckResult struct {
	Timestamp     time.Time
	FoundProducts []Product
}

// --- HELPER FUNCTIONS ---
var nonAlphanumericRegex = regexp.MustCompile(`[^a-zA-Z0-9 ]+`)
var spaceRegex = regexp.MustCompile(`\s+`)

func slugify(s string) string {
	s = strings.ToLower(s)
	s = nonAlphanumericRegex.ReplaceAllString(s, "")
	s = spaceRegex.ReplaceAllString(s, "-")
	return s
}

func constructFullURL(p Product) string {
	productSlug := slugify(p.ProductName)
	return fmt.Sprintf("https://www.firstcry.com/hot-wheels/%s/%s/product-detail", productSlug, p.ProductID)
}

func sendTelegramMessage(chatID, message string) {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", TELEGRAM_BOT_TOKEN)
	payload := url.Values{}
	payload.Set("chat_id", chatID)
	payload.Set("text", message)
	payload.Set("parse_mode", "HTML")
	_, err := http.PostForm(apiURL, payload)
	if err != nil {
		log.Printf("❌ Failed to send Telegram message: %v", err)
	}
}

func loadSeenItems() {
	data, _ := os.ReadFile(SEEN_ITEMS_FILE)
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if line != "" {
			seenItems[line] = true
		}
	}
}

func saveNewItem(productInfoID string) {
	f, err := os.OpenFile(SEEN_ITEMS_FILE, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("Error opening file for writing: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(productInfoID + "\n"); err != nil {
		log.Printf("Error writing to file: %v", err)
	}
}

func startKeepAlive() {
	appURL := "https://hh2-uaol.onrender.com"
	
	go func() {
		log.Println("⏰ Keep-alive will start in 2 minutes...")
		time.Sleep(2 * time.Minute)
		
		ticker := time.NewTicker(8 * time.Minute)
		defer ticker.Stop()
		
		log.Printf("🔄 Keep-alive service started, pinging: %s", appURL)
		
		client := &http.Client{Timeout: 30 * time.Second}
		
		resp, err := client.Get(appURL + "/ping")
		if err != nil {
			log.Printf("⚠️ Initial keep-alive ping failed: %v", err)
		} else {
			resp.Body.Close()
			log.Printf("✅ Initial keep-alive ping successful (status: %d)", resp.StatusCode)
		}
		
		for {
			select {
			case <-ticker.C:
				resp, err := client.Get(appURL + "/ping")
				if err != nil {
					log.Printf("⚠️ Keep-alive ping failed: %v", err)
				} else {
					resp.Body.Close()
					log.Printf("✅ Keep-alive ping successful (status: %d)", resp.StatusCode)
				}
			}
		}
	}()
}

// --- CORE API LOGIC (OPTIMIZED WITH PARALLEL FETCHING) ---
func fetchAndParseAPI(apiURL string) ([]Product, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Referer", "https://www.firstcry.com/")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("bad status code: %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var outer OuterEnvelope
	if err := json.Unmarshal(body, &outer); err != nil {
		var altOuter map[string]interface{}
		if err2 := json.Unmarshal(body, &altOuter); err2 == nil {
			if respStr, ok := altOuter["ProductResponse"].(string); ok {
				outer.ProductResponse = respStr
			} else {
				return nil, err
			}
		} else {
			return nil, err
		}
	}
	if outer.ProductResponse == "" {
		return []Product{}, nil
	}
	var inner InnerData
	if err := json.Unmarshal([]byte(outer.ProductResponse), &inner); err != nil {
		return nil, err
	}
	return inner.Products, nil
}

// 🚀 OPTIMIZED: PARALLEL API FETCHING
func getAllProductsFromAPI() ([]Product, error) {
	var allProducts []Product
	var seenPInfIDs = make(map[string]bool)
	var mu sync.Mutex
	var wg sync.WaitGroup
	
	productsChan := make(chan []Product, PAGES_TO_SCAN)
	errorsChan := make(chan error, PAGES_TO_SCAN)
	
	// Fetch Page 1 in parallel
	wg.Add(1)
	go func() {
		defer wg.Done()
		products, err := fetchAndParseAPI(API_URL_PAGE_1)
		if err != nil {
			errorsChan <- fmt.Errorf("page 1: %w", err)
			return
		}
		productsChan <- products
	}()
	
	// Fetch Pages 2-6 in parallel
	for i := 2; i <= PAGES_TO_SCAN; i++ {
		wg.Add(1)
		pageNum := i
		go func() {
			defer wg.Done()
			pagingURL := fmt.Sprintf(API_URL_PAGING_TEMPLATE, pageNum)
			products, err := fetchAndParseAPI(pagingURL)
			if err != nil {
				errorsChan <- fmt.Errorf("page %d: %w", pageNum, err)
				return
			}
			productsChan <- products
		}()
	}
	
	// Close channels after all goroutines complete
	go func() {
		wg.Wait()
		close(productsChan)
		close(errorsChan)
	}()
	
	// Collect errors (log but don't fail completely)
	var criticalError error
	go func() {
		for err := range errorsChan {
			log.Printf("❌ API fetch error: %v", err)
			if criticalError == nil {
				criticalError = err
			}
		}
	}()
	
	// Collect all products and deduplicate
	for products := range productsChan {
		mu.Lock()
		for _, p := range products {
			if !seenPInfIDs[p.ProductInfoID] {
				allProducts = append(allProducts, p)
				seenPInfIDs[p.ProductInfoID] = true
			}
		}
		mu.Unlock()
	}
	
	log.Printf("✅ Parallel fetch complete: %d unique products found", len(allProducts))
	return allProducts, criticalError
}

// --- CORE LOGIC ---
func initializeBaseline() {
	log.Println("No baseline file found. Performing definitive multi-API scan...")
	products, err := getAllProductsFromAPI()
	if err != nil {
		log.Printf("❌ Fatal error during baseline creation: %v", err)
		return
	}
	var initialItems []string
	for _, p := range products {
		if p.StockStatus != "0" {
			initialItems = append(initialItems, p.ProductInfoID)
		}
	}
	content := strings.Join(initialItems, "\n")
	os.WriteFile(SEEN_ITEMS_FILE, []byte(content), 0644)
	log.Printf("✅ Baseline created with %d IN-STOCK items.", len(initialItems))
}

func checkForNewItems() []Product {
	log.Printf("🔎 (%s) Performing definitive multi-API check...", time.Now().Format("15:04:05"))
	var newProductsFound []Product
	allProducts, err := getAllProductsFromAPI()
	if err != nil {
		log.Printf("❌ Error getting all products: %v", err)
		sendTelegramMessage(ADMIN_CHAT_ID, fmt.Sprintf("⚠️ Bot encountered an error: %v", err))
		return newProductsFound
	}

	log.Printf("... Total products/variations found this check: %d", len(allProducts))
	for _, p := range allProducts {
		if p.StockStatus == "0" {
			continue
		}

		uniqueID := p.ProductInfoID

		mutex.Lock()
		seen := seenItems[uniqueID]
		mutex.Unlock()

		if !seen {
			log.Printf("🚨 NEW ITEM FOUND: %s", p.ProductName)
			newProductsFound = append(newProductsFound, p)

			fullURL := constructFullURL(p)
			message := fmt.Sprintf(
				"<b>🔥 New Hot Wheels Listing!</b>\n\n<b>Name:</b> %s\n<b>Price:</b> %s\n\n<b>Link:</b> <a href='%s'>Click Here</a>",
				p.ProductName, p.Price, fullURL,
			)
			sendTelegramMessage(TELEGRAM_CHAT_ID, message)

			saveNewItem(uniqueID)
			mutex.Lock()
			seenItems[uniqueID] = true
			mutex.Unlock()
		}
	}
	return newProductsFound
}

func scraperWorker(stop chan struct{}) {
	initialFinds := checkForNewItems()
	mutex.Lock()
	checkHistory = append(checkHistory, CheckResult{Timestamp: time.Now(), FoundProducts: initialFinds})
	mutex.Unlock()
	if len(initialFinds) == 0 {
		log.Println("...No new items found.")
		sendTelegramMessage(TELEGRAM_CHAT_ID, "✅ No new listings found on initial check.")
	}
	for {
		mutex.Lock()
		interval := checkInterval
		paused := isPaused
		mutex.Unlock()
		select {
		case <-time.After(interval):
			if !paused {
				newlyFoundProducts := checkForNewItems()
				mutex.Lock()
				checkHistory = append(checkHistory, CheckResult{Timestamp: time.Now(), FoundProducts: newlyFoundProducts})
				if len(checkHistory) > 10 {
					checkHistory = checkHistory[1:]
				}
				isMuted := heartbeatMuted
				currentInterval := checkInterval
				mutex.Unlock()
				if len(newlyFoundProducts) == 0 {
					log.Println("...No new items found.")
					if !isMuted {
						sendTelegramMessage(TELEGRAM_CHAT_ID, fmt.Sprintf("✅ No new listings found. Next check in ~%.0f seconds.", currentInterval.Seconds()))
					}
				}
			} else {
				log.Println("...Scraper is paused.")
			}
		case <-stop:
			log.Println("Scraper worker shutting down.")
			return
		}
	}
}

func commandListenerWorker(stop chan struct{}) {
	log.Println("🤖 Command listener started.")
	var lastUpdateID int
	for {
		apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=10", TELEGRAM_BOT_TOKEN, lastUpdateID+1)
		resp, err := http.Get(apiURL)
		if err != nil {
			log.Printf("Error getting updates: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var updates TelegramUpdateResponse
		json.Unmarshal(body, &updates)
		for _, update := range updates.Result {
			lastUpdateID = update.UpdateID
			if update.Message.Text == "" || update.Message.Chat.ID == 0 {
				continue
			}
			chatIDStr := strconv.FormatInt(update.Message.Chat.ID, 10)
			if chatIDStr != ADMIN_CHAT_ID {
				sendTelegramMessage(chatIDStr, "Sorry, you are not authorized.")
				continue
			}
			parts := strings.Fields(update.Message.Text)
			command := parts[0]
			switch command {
			case "/start":
				mutex.Lock()
				isPaused = false
				mutex.Unlock()
				sendTelegramMessage(ADMIN_CHAT_ID, "▶️ Bot resumed.")
			case "/pause":
				mutex.Lock()
				isPaused = true
				mutex.Unlock()
				sendTelegramMessage(ADMIN_CHAT_ID, "⏸️ Bot paused.")
			case "/stop":
				sendTelegramMessage(ADMIN_CHAT_ID, "🛑 Stopping bot...")
				close(stop)
				return
			case "/mute":
				mutex.Lock()
				heartbeatMuted = true
				mutex.Unlock()
				sendTelegramMessage(ADMIN_CHAT_ID, "🔕 Heartbeat notifications muted.")
			case "/unmute":
				mutex.Lock()
				heartbeatMuted = false
				mutex.Unlock()
				sendTelegramMessage(ADMIN_CHAT_ID, "🔔 Heartbeat notifications enabled.")
			case "/setinterval":
				if len(parts) > 1 {
					i, err := strconv.Atoi(parts[1])
					if err == nil && i >= 5 {
						mutex.Lock()
						checkInterval = time.Duration(i) * time.Second
						mutex.Unlock()
						sendTelegramMessage(ADMIN_CHAT_ID, fmt.Sprintf("✅ Interval set to %d seconds.", i))
					} else {
						sendTelegramMessage(ADMIN_CHAT_ID, "❌ Invalid interval (minimum 5 seconds).")
					}
				} else {
					sendTelegramMessage(ADMIN_CHAT_ID, "Usage: /setinterval <seconds>")
				}
			case "/status":
				mutex.Lock()
				status := "▶️ Running"
				if isPaused {
					status = "⏸️ Paused"
				}
				hbStatus := "🔔 Active"
				if heartbeatMuted {
					hbStatus = "🔕 Muted"
				}
				interval := checkInterval
				mutex.Unlock()
				sendTelegramMessage(ADMIN_CHAT_ID, fmt.Sprintf("<b>Bot Status:</b>\n%s\nCheck Interval: %.0f seconds\nHeartbeat: %s", status, interval.Seconds(), hbStatus))
			case "/recent":
				var sb strings.Builder
				sb.WriteString("<b>🔎 Recent Finds (Last 10 Checks)</b>\n\n")
				mutex.Lock()
				totalFound := 0
				for i := len(checkHistory) - 1; i >= 0; i-- {
					result := checkHistory[i]
					if len(result.FoundProducts) > 0 {
						totalFound += len(result.FoundProducts)
						loc, _ := time.LoadLocation("Asia/Kolkata")
						sb.WriteString(fmt.Sprintf("<b><u>Found at %s:</u></b>\n", result.Timestamp.In(loc).Format("03:04 PM, Jan 02")))
						for _, p := range result.FoundProducts {
							fullURL := constructFullURL(p)
							sb.WriteString(fmt.Sprintf("- <a href='%s'>%s</a>\n", fullURL, p.ProductName))
						}
						sb.WriteString("\n")
					}
				}
				mutex.Unlock()
				if totalFound == 0 {
					sb.WriteString("No new products found in the last 10 checks.")
				}
				sendTelegramMessage(ADMIN_CHAT_ID, sb.String())
			}
		}
	}
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("--- 🔥 Hot Wheels Hunter Started (5-Second Parallel Version) 🔥 ---")

	// Add HTTP server for Render deployment
	go func() {
		port := os.Getenv("PORT")
		if port == "" {
			port = "8080"
		}
		
		http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("🔥 Hot Wheels Hunter is running! (5-second intervals) 🔥"))
		})
		
		http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
			mutex.Lock()
			status := "running"
			if isPaused {
				status = "paused"
			}
			interval := checkInterval
			itemCount := len(seenItems)
			mutex.Unlock()
			
			response := fmt.Sprintf(`{
				"status": "%s",
				"check_interval_seconds": %.0f,
				"tracked_items": %d,
				"bot": "Hot Wheels Hunter (Optimized)",
				"timestamp": "%s"
			}`, status, interval.Seconds(), itemCount, time.Now().Format("2006-01-02 15:04:05"))
			
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(response))
		})
		
		http.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("pong"))
		})
		
		log.Printf("🌐 HTTP server starting on port %s", port)
		if err := http.ListenAndServe(":"+port, nil); err != nil {
			log.Printf("❌ HTTP server error: %v", err)
		}
	}()
	
	// Start keep-alive service
	startKeepAlive()
	
	if _, err := os.Stat(SEEN_ITEMS_FILE); os.IsNotExist(err) {
		initializeBaseline()
	}
	loadSeenItems()
	log.Printf("✅ Loaded existing baseline with %d items.", len(seenItems))
	stop := make(chan struct{})
	go scraperWorker(stop)
	go commandListenerWorker(stop)
	sendTelegramMessage(ADMIN_CHAT_ID, "🚀 Bot is online and running! (5-second intervals)")
	<-stop
	log.Println("--- Bot has been shut down. ---")
}
