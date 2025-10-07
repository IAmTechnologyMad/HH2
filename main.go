package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// --- CONFIGURATION ---
const (
	// FirstCry API URLs
	API_URL_PAGE_1 = "https://www.firstcry.com/svcs/SearchResult.svc/GetSearchResultProductsFilters?PageNo=1&PageSize=100&SortExpression=NewArrivals&OnSale=5&SearchString=brand&SubCatId=&BrandId=&Price=&Age=&Color=&OptionalFilter=&OutOfStock=&Type1=&Type2=&Type3=&Type4=&Type5=&Type6=&Type7=&Type8=&Type9=&Type10=&Type11=&Type12=&Type13=&Type14=&Type15=&combo=&discount=&searchwithincat=&ProductidQstr=&searchrank=&pmonths=&cgen=&PriceQstr=&DiscountQstr=&MasterBrand=113&sorting=&Rating=&Offer=&skills=&material=&curatedcollections=&measurement=&gender=&exclude=&premium=&pcode=680566&isclub=0&deliverytype="
	API_URL_PAGING_TEMPLATE = "https://www.firstcry.com/svcs/SearchResult.svc/GetSearchResultProductsPaging?PageNo=%d&PageSize=20&SortExpression=NewArrivals&OnSale=5&SearchString=brand&SubCatId=&BrandId=&Price=&Age=&Color=&OptionalFilter=&OutOfStock=&Type1=&Type2=&Type3=&Type4=&Type5=&Type6=&Type7=&Type8=&Type9=&Type10=&Type11=&Type12=&Type13=&Type14=&Type15=&combo=&discount=&searchwithincat=&ProductidQstr=&searchrank=&pmonths=&cgen=&PriceQstr=&DiscountQstr=&sorting=&MasterBrand=113&Rating=&Offer=&skills=&material=&curatedcollections=&measurement=&gender=&exclude=&premium=&pcode=680566&isclub=0&deliverytype="
	PAGES_TO_SCAN           = 6 // Total pages (1 filter + 5 paging)

	// Telegram
	TELEGRAM_BOT_TOKEN = "8222224289:AAFDgJ2C0KSTks9lLhPKtUtR1KzqraNkybI"
	TELEGRAM_CHAT_ID   = "-4985438208"
	ADMIN_CHAT_ID      = "837428747"

	// Seen items file
	SEEN_ITEMS_FILE = "seen_hotwheels_go.txt"

	// Scan intervals
	QuickCheckInterval = 3 * time.Second
	FullScanInterval   = 1 * time.Minute
)

var (
	seenItems    = make(map[string]bool)
	lastFullScan = time.Now()
	mu           sync.Mutex
)

// --- STRUCTS ---
type Product struct {
	Pid         string `json:"Pid"`
	ProductName string `json:"ProductName"`
	Price       string `json:"ProductFinalPrice"`
	ProductURL  string `json:"ProductUrl"`
	ImageURL    string `json:"ProductImage"`
}

// --- FETCH API ---
func fetchAndParseAPI(url string) ([]Product, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetch error: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var data struct {
		ProductList []Product `json:"ProductList"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("parse error: %v", err)
	}
	return data.ProductList, nil
}

// --- TELEGRAM ---
func sendTelegramMessage(chatID string, message string) {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", TELEGRAM_BOT_TOKEN)
	body := fmt.Sprintf("chat_id=%s&text=%s&parse_mode=Markdown", chatID, message)
	http.Post(apiURL, "application/x-www-form-urlencoded", strings.NewReader(body))
}

// --- FILE HANDLING ---
func loadSeenItems() {
	file, err := os.ReadFile(SEEN_ITEMS_FILE)
	if err != nil {
		log.Println("No previous seen file found.")
		return
	}
	lines := strings.Split(string(file), "\n")
	for _, line := range lines {
		if line != "" {
			seenItems[line] = true
		}
	}
}

func saveSeenItems() {
	mu.Lock()
	defer mu.Unlock()

	var data []string
	for id := range seenItems {
		data = append(data, id)
	}
	os.WriteFile(SEEN_ITEMS_FILE, []byte(strings.Join(data, "\n")), 0644)
}

func clearSeenItems() {
	mu.Lock()
	defer mu.Unlock()

	seenItems = make(map[string]bool)
	os.WriteFile(SEEN_ITEMS_FILE, []byte(""), 0644)
	log.Println("🧹 Seen items cleared by admin command.")
	sendTelegramMessage(ADMIN_CHAT_ID, "✅ Seen items cleared successfully.")
}

// --- PROCESSING ---
func processProducts(products []Product) {
	mu.Lock()
	defer mu.Unlock()

	for _, p := range products {
		if !seenItems[p.Pid] {
			seenItems[p.Pid] = true
			message := fmt.Sprintf(
				"🚨 *New Drop Detected!*\n\n🧾 *%s*\n💰 *Price:* ₹%s\n🔗 [View Product](%s)\n🖼️ %s",
				p.ProductName, p.Price, p.ProductURL, p.ImageURL)
			go sendTelegramMessage(TELEGRAM_CHAT_ID, message)
			log.Printf("[NEW] %s", p.ProductName)
		}
	}
	saveSeenItems()
}

// --- SCANS ---
func runFullScan() {
	log.Println("🧩 Running full multi-page scan...")
	var wg sync.WaitGroup

	for page := 1; page <= PAGES_TO_SCAN; page++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			var url string
			if p == 1 {
				url = API_URL_PAGE_1
			} else {
				url = fmt.Sprintf(API_URL_PAGING_TEMPLATE, p)
			}
			products, err := fetchAndParseAPI(url)
			if err != nil {
				log.Println("Error fetching:", url, err)
				return
			}
			processProducts(products)
		}(page)
	}

	wg.Wait()
	log.Println("✅ Full scan complete.")
}

func runQuickScan() {
	log.Println("⚡ Running quick scan (page 1)...")
	products, err := fetchAndParseAPI(API_URL_PAGE_1)
	if err != nil {
		log.Println("Quick scan error:", err)
		return
	}
	processProducts(products)
}

// --- KEEP ALIVE SERVER + TELEGRAM COMMANDS ---
func startKeepAliveServer() {
	http.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "pong")
	})

	// Telegram webhook (optional if using polling)
	http.HandleFunc("/telegram", func(w http.ResponseWriter, r *http.Request) {
		var update struct {
			Message struct {
				Text string `json:"text"`
				Chat struct {
					ID int64 `json:"id"`
				} `json:"chat"`
				From struct {
					ID int64 `json:"id"`
				} `json:"from"`
			} `json:"message"`
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &update)

		if update.Message.Text == "/clear" && fmt.Sprint(update.Message.From.ID) == ADMIN_CHAT_ID {
			clearSeenItems()
		}

		w.WriteHeader(http.StatusOK)
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "10000"
	}
	log.Printf("🌐 Keep-alive server running on port %s", port)
	http.ListenAndServe(":"+port, nil)
}

// --- MAIN ---
func main() {
	log.Println("🚀 Hot Wheels Monitor Started!")
	loadSeenItems()

	// Start keep-alive
	go startKeepAliveServer()

	// Quick scan loop
	go func() {
		for {
			runQuickScan()
			time.Sleep(QuickCheckInterval)
		}
	}()

	// Full scan loop
	for {
		if time.Since(lastFullScan) >= FullScanInterval {
			runFullScan()
			lastFullScan = time.Now()
		}
		time.Sleep(5 * time.Second)
	}
}
