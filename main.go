package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/tebeka/selenium"
)

type Row struct {
	Date       time.Time
	UnixTime   int64
	Rank       int64
	Name       string
	Symbol     string
	MarketCap  sql.NullFloat64
	Price      sql.NullFloat64
	Supply     sql.NullInt64
	Volume     sql.NullFloat64
	HourChange sql.NullFloat64
	DayChange  sql.NullFloat64
	WeekChange sql.NullFloat64
}

// #region Configs
// Time delay between scrolls. May cause issues fully loading table data if it is too low
var scrollDelay time.Duration = 600 * time.Millisecond

// Multiple of viewport height for scrolling. May cause issues fully loading table data if it is too high
const viewportScrollMult float64 = 1.4

// Time delay between pressing the "Load More" button. May make program skip data if it is too low
const loadMoreDelay time.Duration = 2 * time.Second

// Name of table in your database. Will be created if it doesn't exist
const tableName string = "marketcap_snapshots"

// Skips coins with no marketcap listed on CoinMarketCap (CMC)
const skipNoMcap bool = true

// Time after which program will timeout and main loop will restart
const timeout time.Duration = 60 * time.Minute

// Number of rows per snapshot page that the program will process and enter to database.
//
// Zero or negative for no max
const maxRows = 3000

const exportCSV bool = true

// #endregion

func main() {
	// #region Load db.env environment variables
	err := godotenv.Load("db.env")
	if err != nil {
		log.Fatal("Error loading .env file")
	}

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		fmt.Println("Error getting filepath")
		return
	}
	dirPath := filepath.Dir(filename) // #endregion

	// #region Connect to database
	ctx := context.Background()
	connStr := "postgres://" + os.Getenv("DB_USER") + ":" + os.Getenv("DB_PASS") + "@" + os.Getenv("DB_HOST") + ":" + os.Getenv("DB_PORT") + "/" + os.Getenv("DB_NAME")
	dbpool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		log.Printf("Unable to connect to database: %v\n", os.Stderr)
		os.Exit(1)
	} else {
		log.Println("DB connected successfully")
	}
	defer dbpool.Close() // #endregion

	// #region Try to select the latest date on table. If table doesn't ...
	// exist, create table and initialize starting date to April 28th, 2013
	// (the first snapshot on coinmarketcap)
	queryLastDate := dbpool.QueryRow(ctx, `SELECT snapshot_date FROM `+tableName+` ORDER BY snapshot_date DESC LIMIT 1`)
	var date time.Time
	err = queryLastDate.Scan(&date)
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			date = time.Date(2013, 4, 28, 0, 0, 0, 0, time.UTC)
			log.Println("Table does not exist. Creating new table.")
			_, err = dbpool.Exec(ctx, `
				CREATE TABLE IF NOT EXISTS `+tableName+` (
					snapshot_date DATE,
					unix_time BIGINT,
					rank INTEGER,
					name VARCHAR(255),
					symbol VARCHAR(30),
					market_cap DECIMAL,
					price DECIMAL,
					circulating_supply BIGINT,
					volume_24h DECIMAL,
					percent_change_1h DECIMAL(7, 2),
					percent_change_24h DECIMAL(7, 2),
					percent_change_7d DECIMAL(7, 2),
				
					PRIMARY KEY (snapshot_date, rank, symbol)
				);
			`)
			if err != nil {
				log.Fatal("Error creating table |", err)
			}
		} else {
			log.Fatal("Error querying table |", err)
		}
	} else {
		log.Println("Most recent date is", date)
		date = date.AddDate(0, 0, 7)
	} // #endregion

	// Begin main program loop to iterate over every snapshot while watching for timeout
	timer := time.NewTicker(timeout)
main:
	for {
		select {
		case <-timer.C:
			log.Println("Program timed out. Possible chrome browser OOM error, restarting loop")
			timer.Reset(timeout)
			continue
		default:
		snapshotsLoop:
			for date.Before(time.Now()) {
				// #region Connect to the WebDriver instance running locally
				opts := []selenium.ServiceOption{}
				chromeDriverPath := "./chromedriver.exe"
				service, err := selenium.NewChromeDriverService(chromeDriverPath, 4444, opts...)
				if err != nil {
					if strings.Contains(err.Error(), "file does not exist") {
						log.Println("Error finding chromedriver.exe | ", err)
						fmt.Println("Enter chromedriver.exe absolute path:")
						fmt.Scanln(&chromeDriverPath)
						service, err = selenium.NewChromeDriverService(chromeDriverPath, 4444, opts...)
						if err != nil {
							log.Fatal("Error finding chromedriver.exe. Check file path and restart program | ", err)
						}
					} else {
						log.Fatal("Error starting ChromeDriver service | ", err)
					}
				}
				defer service.Stop()

				caps := selenium.Capabilities{"browserName": "chrome"}
				wd, err := selenium.NewRemote(caps, fmt.Sprintf("http://localhost:%d/wd/hub", 4444))
				if err != nil {
					log.Fatal("Failed to open session:", err)
				}
				defer wd.Quit()
				log.Println("ChromeDriver server started successfully") // #endregion

				// #region Navigate to page and fully load by clicking buttons
				// Navigate to the webpage
				log.Println("Beginning parse for snapshot | ", date)
				url := fmt.Sprintf("https://coinmarketcap.com/historical/%d%02d%02d/", date.Year(), date.Month(), date.Day())
				log.Println(url)
				err = wd.Get(url)
				if err != nil {
					fmt.Println("Failed to load page:", err)
				}

				// Wait for the page to load (looks for <div class="container cmc-main-section">)
				condition := func(wd selenium.WebDriver) (bool, error) {
					_, err := wd.FindElement(selenium.ByCSSSelector, "div.container.cmc-main-section")
					if err != nil {
						if err.Error() == "no such element" {
							return false, nil
						}
						return false, err
					}
					return true, nil
				}
				wd.Wait(condition)
				log.Println("Page loaded")

				// Find the "Reject All" button and click it
				clickRejectAll(wd)
				countLoadMoreClicked := 0
				maxClicks := int(math.Ceil(maxRows/200) - 1) // divide maxRows by 200 as Load More button populates 200 entries per click
				for maxRows <= 0 || countLoadMoreClicked < maxClicks {
					// Find the "Load More" button and click it
					button, err := wd.FindElement(selenium.ByCSSSelector, "div.cmc-table-listing__loadmore > button[type='button']")
					if err != nil {
						if strings.Contains(err.Error(), "no such element") {
							log.Println("\"Load More\" button not found")
							break
						} else {
							log.Fatal("Error finding \"Load More\" button | ", err)
						}
					} else {
						err = button.Click()
						if err != nil {
							if strings.Contains(err.Error(), "click intercepted") {
								clickRejectAll(wd)
								continue
							} else {
								log.Fatal("Error clicking \"Load More\" button | ", err)
							}
						} else {
							log.Println("\"Load More\" button clicked")
						}
						countLoadMoreClicked += 1
						// Weird edge case for date = 2016-07-03 where Load More button
						// can be clicked infinitely. Suspect it is because there are
						// exactly 600 cryptos and page stops at 600. Server side has
						// off-by-one error?
						//
						// NOTE: Hard coded break after 100 clicks won't be future proof
						// if CMC adds more than 20,000 cryptos
						if countLoadMoreClicked > 100 {
							break
						}
						time.Sleep(loadMoreDelay)
					}
				} // #endregion

				scrollPage(scrollDelay, wd)

				// #region Iterate theads and find column indexes
				colIndexes := make(map[string]int)
				theads, err := wd.FindElements(selenium.ByCSSSelector, "thead")
				if err != nil {
					log.Println("Failed to find thead | ", err)
				}
				if len(theads) == 0 {
					// page likely didn't load due to rate limiting, try again in 5 minutes
					wd.DeleteAllCookies()
					log.Println("Error finding theads, restarting loop in 5 minutes")
					time.Sleep(300 * time.Second)
					continue snapshotsLoop
				}
				thead := theads[2]
				columns, err := thead.FindElements(selenium.ByCSSSelector, "th")
				if err != nil {
					log.Println("Failed to find columns from thead | ", err)
				}
				for i, column := range columns {
					columnText, err := column.Text()
					if err != nil {
						log.Println("Error converting column to text, restarting loop | ", err)
						continue snapshotsLoop
					}
					colIndexes[columnText] = i
				}
				if _, ok := colIndexes["Rank"]; !ok {
					log.Fatal("Error loading colIndexes \"Rank\" not found | ", colIndexes)
				}

				var queuedRows []Row

				// Find and iterate over table rows, append them to queuedRows
				tbody, err := wd.FindElement(selenium.ByCSSSelector, "tbody")
				if err != nil {
					log.Println("Failed to find body, restarting loop | ", err)
					continue snapshotsLoop
				}
				rows, err := tbody.FindElements(selenium.ByCSSSelector, "tr")
				if err != nil {
					log.Println("Error finding row elements, restarting loop | ", err)
					continue snapshotsLoop
				} // #endregion

				countRowsInserted := 0
			rowsLoop:
				for _, row := range rows {
					// #region Find and convert cells to data types for Row struct and append to slice
					cells, err := row.FindElements(selenium.ByCSSSelector, "td")
					if err != nil {
						log.Println("Error finding cell elements, restarting loop | ", err)
						wd.Quit()
						continue snapshotsLoop
					}

					var rank int64
					var name string
					var symbol string
					var marketCap float64
					var mcapNotNull bool
					var price float64
					var priceNotNull bool
					var supply int64
					var supplyNotNull bool
					var volume float64
					var volumeNotNull bool
					var hourChange float64
					var hourNotNull bool
					var dayChange float64
					var dayNotNull bool
					var weekChange float64
					var weekNotNull bool
					var b strings.Builder
					if len(cells) < len(colIndexes) {
						log.Println("index length error, scroll speed may be too fast. increasing scroll delay and restarting loop | ", err)
						queuedRows = nil
						scrollDelay = scrollDelay + 50*time.Millisecond
						scrollPage(scrollDelay, wd)
						continue rowsLoop
					}
					if marketCapTxt, err := cells[colIndexes["Market Cap"]].Text(); err != nil {
						log.Fatal("Error converting marketCap cell to text | ", err)
					} else {
						if marketCapTxt == "--" || marketCapTxt == "" {
							if skipNoMcap {
								continue
							} else {
								marketCap = 0.0
								mcapNotNull = false
							}
						} else {
							mcapNotNull = true
							for _, ch := range marketCapTxt {
								switch ch {
								case '$', ',':
									continue
								default:
									b.WriteRune(ch)
								}
							}
							marketCapTxt = b.String()
							if marketCap, err = strconv.ParseFloat(marketCapTxt, 64); err != nil {
								log.Fatal("ParseFloat error, marketCap | ", err)
							}
						}
					}
					if rankTxt, err := cells[colIndexes["Rank"]].Text(); err != nil {
						log.Fatal("Error converting cell to text | ", err)
					} else {
						if rankTxt == "" {
							log.Printf("Error loading \"Rank\" column for row %v, restarting loop for this snapshot date %v", row, date)
							wd.Quit()
							continue snapshotsLoop
						}
						if rank, err = strconv.ParseInt(rankTxt, 10, 64); err != nil {
							log.Fatal("Error converting string to int | ", err)
						}
					}
					if name, err = cells[colIndexes["Name"]].Text(); err != nil {
						log.Fatal("Error converting name cell to text | ", err)
					}
					if symbol, err = cells[colIndexes["Symbol"]].Text(); err != nil {
						log.Fatal("Error converting symbol cell to text | ", err)
					}
					if priceTxt, err := cells[colIndexes["Price"]].Text(); err != nil {
						log.Fatal("Error converting price cell to text | ", err)
					} else {
						b.Reset()
						for _, ch := range priceTxt {
							switch ch {
							case '$', ',':
								continue
							default:
								b.WriteRune(ch)
							}
						}
						priceTxt = b.String()
						if priceTxt == "" || priceTxt == "--" {
							price = 0.0
							priceNotNull = false
						} else {

							if price, err = strconv.ParseFloat(priceTxt, 64); err != nil {
								log.Fatal("ParseFloat error, price | ", err)
							}
							priceNotNull = true
						}
					}
					if supplyTxt, err := cells[colIndexes["Circulating Supply"]].Text(); err != nil {
						log.Fatal("Error converting supply cell to text | ", err)
					} else {
						supplyTxt, _, _ = strings.Cut(supplyTxt, " ")
						if supplyTxt == "" || supplyTxt == "?" {
							supplyNotNull = false
							supply = 0
						} else {
							supplyNotNull = true
							b.Reset()
							for _, ch := range supplyTxt {
								switch ch {
								case ',', ' ':
									continue
								default:
									b.WriteRune(ch)
								}
							}
							supplyTxt = b.String()
							if supply, err = strconv.ParseInt(supplyTxt, 10, 64); err != nil {
								if strings.Contains(err.Error(), "value out of range") {
									log.Println("supply too large for int64, entering null")
									supply = 0
									supplyNotNull = false
								} else {
									log.Fatal("ParseInt error, supply | ", err)
								}
							}
						}
					}
					if volIndex, ok := colIndexes["volume (24h)"]; ok {
						if volumeTxt, err := cells[volIndex].Text(); err != nil {
							log.Fatal("Error converting volume cell to text | ", err)
						} else {
							b.Reset()
							for _, ch := range volumeTxt {
								switch ch {
								case '$', ',':
									continue
								default:
									b.WriteRune(ch)
								}
							}
							volumeTxt = b.String()
							if volumeTxt == "--" || volumeTxt == "" {
								volumeNotNull = false
								volume = 0
							} else {
								volumeNotNull = true
								if volume, err = strconv.ParseFloat(volumeTxt, 64); err != nil {
									log.Fatal("ParseFloat error, volume | ", err)
								}
							}
						}
					} else {
						volumeNotNull = false
						volume = 0
					}
					hourChange, hourNotNull = percTxtToFloat64(cells[7].Text())
					dayChange, dayNotNull = percTxtToFloat64(cells[8].Text())
					weekChange, weekNotNull = percTxtToFloat64(cells[9].Text())

					newRow := Row{
						Date:     date,
						UnixTime: date.Unix(),
						Rank:     rank,
						Name:     name,
						Symbol:   symbol,
						MarketCap: sql.NullFloat64{
							Float64: marketCap,
							Valid:   mcapNotNull,
						},
						Price: sql.NullFloat64{
							Float64: price,
							Valid:   priceNotNull,
						},
						Supply: sql.NullInt64{
							Int64: supply,
							Valid: supplyNotNull,
						},
						Volume: sql.NullFloat64{
							Float64: volume,
							Valid:   volumeNotNull,
						},
						HourChange: sql.NullFloat64{
							Float64: hourChange,
							Valid:   hourNotNull,
						},
						DayChange: sql.NullFloat64{
							Float64: dayChange,
							Valid:   dayNotNull,
						},
						WeekChange: sql.NullFloat64{
							Float64: weekChange,
							Valid:   weekNotNull,
						},
					}
					queuedRows = append(queuedRows, newRow)
					countRowsInserted += 1
					if countRowsInserted >= maxRows {
						break
					} // #endregion
				}

				// #region Batch insert rows to database and prepare for next iteration
				// #region DELETE exit program if there are 3000 or more valid rows
				if len(queuedRows) >= 3000 {
					log.Fatalf("There are 3000 or more rows at snapshot date %s. Change const maxRows", date.Format("2006-01-02"))
				} // #endregion
				log.Println("Batch inserting rows")
				batchInsertRows(queuedRows, ctx, dbpool)
				log.Printf("Successfully batch inserted %d rows", len(queuedRows))
				// add 7 days to next entry
				date = date.AddDate(0, 0, 7)
				timer.Reset(timeout)
				wd.Quit() // #endregion

				// #region Exit main loop when entries are caught up to current date
				if date.After(time.Now()) {
					break main
				} // #endregion
			}
		}
	}

	// #region Export to CSV after program finished
	log.Println("Scraping complete")
	if exportCSV {
		copyQueryString := `COPY marketcap_snapshots TO '` + dirPath + `\` + tableName + `_` + time.Now().Format("2006-01-02") + `' DELIMITER ',' CSV HEADER;`
		_, err = dbpool.Exec(ctx, copyQueryString)
		if err != nil {
			log.Fatal("Error exporting to .csv | ", err)
		}
		log.Println("Exported database to .csv")
		// #endregion
	}
}

// From the top, scroll down one frame at a time until it reaches the bottom.
// This loads the dynamically populated data on the page
func scrollPage(scrollDelay time.Duration, wd selenium.WebDriver) {
	var err error
	_, err = wd.ExecuteScript("window.scrollTo(0, 0);", nil)
	if err != nil {
		fmt.Println("Failed to scroll to top:", err)
	}
	// Get the height of the viewport (the visible part of the page)
	var viewportHeight interface{}
	viewportHeight, err = wd.ExecuteScript("return window.innerHeight;", nil)
	if err != nil {
		fmt.Println("Failed to get viewport height | ", err)
	}
	// Get the total height of the webpage
	var bodyHeight interface{}
	bodyHeight, err = wd.ExecuteScript("return document.body.scrollHeight;", nil)
	if err != nil {
		fmt.Println("Failed to get body height | ", err)
	}
	// Scroll down slowly, one viewport * viewportScrollMult at a time
	scrollLength := int(viewportHeight.(float64) * viewportScrollMult)
	for i := 0; i < int(bodyHeight.(float64)); i += scrollLength {
		script := fmt.Sprintf("window.scrollBy(0, %d);", scrollLength)

		_, err = wd.ExecuteScript(script, nil)
		if err != nil {
			fmt.Println("Failed to scroll:", err)
		}

		// Wait for a while to let the page load
		time.Sleep(scrollDelay)
	}
	log.Println("End of page reached")
}

// Find and click "Reject All" button
func clickRejectAll(wd selenium.WebDriver) {
	rejectButton, err := wd.FindElement(selenium.ByCSSSelector, "#onetrust-reject-all-handler")
	if err != nil {
		log.Println("\"Reject All\" button not found |")
	} else {
		err = rejectButton.Click()
		if err != nil {
			log.Println("Failed to click reject button | ", err)
		} else {
			log.Println("\"Reject All\" button clicked")
		}
	}
}

// Batch insert processed rows into database
func batchInsertRows(rows []Row, ctx context.Context, dbpool *pgxpool.Pool) {
	queryInsertData := `
		INSERT INTO ` + tableName + ` 
		(snapshot_date, unix_time, rank, name, symbol, market_cap, price, circulating_supply, 
			volume_24h, percent_change_1h, percent_change_24h, percent_change_7d) 
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12);
		`
	batch := &pgx.Batch{}
	for _, row := range rows {
		batch.Queue(queryInsertData, row.Date, row.UnixTime, row.Rank, row.Name, row.Symbol, row.MarketCap, row.Price, row.Supply, row.Volume, row.HourChange, row.DayChange, row.WeekChange)
	}
	br := dbpool.SendBatch(ctx, batch)
	_, err := br.Exec()
	if err != nil {
		log.Fatalf("Unable to execute statement in batch queue %v\n", err)
	}
	log.Println("Successfully batch inserted data")
	err = br.Close()
	if err != nil {
		log.Fatalf("Unable to close batch %v\n", err)
	}
}

// Process rows for percent change cells
func percTxtToFloat64(text string, err error) (float64, bool) {
	if err != nil {
		log.Fatal("percTxtToFloat64 error converting cell to text | ", err)
	}
	if text == "--" || text == "" {
		return 0.0, false
	} else {
		var b strings.Builder
		for _, ch := range text {
			switch ch {
			case '%', ',', '<', '>', ' ':
				continue
			default:
				b.WriteRune(ch)
			}
		}
		text = b.String()
		if percentChange, err := strconv.ParseFloat(text, 64); err != nil {
			log.Fatal("prcTxtToFloat64 ParseFloat error | ", err)
			return 0.0, false
		} else {
			return percentChange, true
		}
	}
}
