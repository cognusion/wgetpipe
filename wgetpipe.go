package main

/*
	Wgetpipe takes a list of fully-qualified URLs over STDIN and Gets them,
	outputting the code, url and elapsed fetch time.

	scans stdin in a goro, spawning up to MAX getters at a time, which stream their
	responses over a channel back to main which formats the output
*/

import (
	"bufio"
	"flag"
	"fmt"
	"github.com/fatih/color"
	"github.com/viki-org/dnscache"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

var (
	MAX        int           // maximum number of outstanding HTTP get requests allowed
	SleepTime  time.Duration // Duration to sleep between GETter spawns
	ErrOnly    bool          // Quiet unless 0 == Code >= 400
	NoColor    bool          // Disable colorizing
	NoDnsCache bool          // Disable DNS caching
	Summary    bool          // Output final stats
	Debug      bool          // Enable debugging
)

type urlCode struct {
	Url  string
	Code int
	Dur  time.Duration
	Err  error
}

func init() {
	flag.IntVar(&MAX, "max", 5, "Maximium in-flight GET requests at a time")
	flag.BoolVar(&ErrOnly, "errorsonly", false, "Only output errors (HTTP Codes >= 400)")
	flag.BoolVar(&NoColor, "nocolor", false, "Don't colorize the output")
	flag.BoolVar(&Summary, "stats", false, "Output stats at the end")
	flag.DurationVar(&SleepTime, "sleep", 0, "Amount of time to sleep between spawning a GETter (e.g. 1ms, 10s)")
	flag.BoolVar(&Debug, "debug", false, "Enable debug output")
	flag.BoolVar(&NoDnsCache, "nodnscache", false, "Disable DNS caching")
	flag.Parse()

	if NoColor {
		color.NoColor = true
	}

	// Sets the default http client to use dnscache, because duh
	if NoDnsCache == false {
		res := dnscache.New(1 * time.Hour)
		http.DefaultClient.Transport = &http.Transport{
			MaxIdleConnsPerHost: 64,
			Dial: func(network string, address string) (net.Conn, error) {
				separator := strings.LastIndex(address, ":")
				ip, _ := res.FetchOneString(address[:separator])
				return net.Dial("tcp", ip+address[separator:])
			},
		}
	}
}

func main() {

	start := time.Now()
	getChan := make(chan string) //, MAX*2)
	rChan := make(chan urlCode)
	doneChan := make(chan bool)
	count := 0
	error4s := 0
	error5s := 0
	errors := 0
	var ticker *time.Ticker

	if Debug {
		// Spawns off an emitter that blurts the length of the three channels every 2s
		ticker = time.NewTicker(time.Second * 2)
		go func(g chan string, r chan urlCode, d chan bool) {
			for _ = range ticker.C {
				fmt.Printf("Get: %d R: %d D: %d\n", len(g), len(r), len(d))
			}
		}(getChan, rChan, doneChan)
	}

	// Spawn off the getters
	for g := 0; g < MAX; g++ {
		go getter(getChan, rChan, doneChan)
	}

	// Block until all the getters are done, and then close rChan
	go func() {
		defer close(rChan)

		for c := 0; c < MAX; c++ {
			<-doneChan
			if Debug {
				fmt.Printf("Done %d/%d\n", c+1, MAX)
			}
		}
	}()

	// spawn off the scanner
	go scanStdIn(getChan)

	// Collate the results
	for i := range rChan {
		count++

		if i.Code == 0 {
			errors++
			color.Red("%d %s %s (%s)\n", i.Code, i.Url, i.Dur.String(), i.Err)
		} else if i.Code < 400 {
			if ErrOnly {
				// skip
				continue
			}
			color.Green("%d %s %s\n", i.Code, i.Url, i.Dur.String())
		} else if i.Code < 500 {
			error4s++
			color.Yellow("%d %s %s\n", i.Code, i.Url, i.Dur.String())
		} else {
			error5s++
			color.Red("%d %s %s\n", i.Code, i.Url, i.Dur.String())
		}
	}

	if Debug {
		// Stops the channel-length emitter
		ticker.Stop()
	}

	elapsed := time.Since(start)

	if Summary {
		e := color.RedString("%d", errors)
		e4 := color.YellowString("%d", error4s)
		e5 := color.RedString("%d", error5s)
		fmt.Printf("\n\nGETs: %d\nErrors: %s\n500 Errors: %s\n400 Errors: %s\nElapsed Time: %s\n", count, e, e5, e4, elapsed.String())
	}
}

// scanStdIn takes a channel to pass inputted strings to,
// and does so until EOF, whereafter it closes the channel
func scanStdIn(getChan chan string) {
	defer close(getChan)

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		getChan <- scanner.Text()
	}
	// POST: we've seen EOF

	if Debug {
		fmt.Println("EOF seen")
	}

}

// getter takes a receive channel, send channel, and done channel,
// running HTTP GETs for anything in the receive channel, returning
// formatted responses to the send channel, and signalling completion
// via the done channel
func getter(getChan chan string, rChan chan urlCode, doneChan chan bool) {
	defer func() { doneChan <- true }()

	for url := range getChan {
		s := time.Now()
		response, err := http.Get(url)
		d := time.Since(s)
		if err != nil {
			// We assume code 0 to be a non-HTTP error
			rChan <- urlCode{url, 0, d, err}
		} else {
			response.Body.Close() // else leak
			rChan <- urlCode{url, response.StatusCode, d, nil}
		}

		if SleepTime > 0 {
			// Zzzzzzz
			time.Sleep(SleepTime)
		}
	}

}
