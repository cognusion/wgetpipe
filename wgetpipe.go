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
	"net/http"
	"os"
	"time"
)

var (
	MAX       int           // maximum number of outstanding HTTP get requests allowed
	SleepTime time.Duration // Duration to sleep between GETter spawns
	ErrOnly   bool          // Quiet unless 0 == Code >= 400
	NoColor   bool          // Disable colorizing
	Summary   bool          // Output final stats
	Debug     bool          // Enable debugging
)

type urlCode struct {
	Url  string
	Code int
	Dur  time.Duration
}

func init() {
	flag.IntVar(&MAX, "max", 5, "Maximium in-flight GET requests at a time")
	flag.BoolVar(&ErrOnly, "errorsonly", false, "Only output errors (HTTP Codes >= 400)")
	flag.BoolVar(&NoColor, "nocolor", false, "Don't colorize the output")
	flag.BoolVar(&Summary, "stats", false, "Output stats at the end")
	flag.DurationVar(&SleepTime, "sleep", 0, "Amount of time to sleep between spawning a GETter (e.g. 1ms, 10s)")
	flag.BoolVar(&Debug, "debug", false, "Enable debug output")
	flag.Parse()

	if NoColor {
		color.NoColor = true
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

	for i := range rChan {
		count++

		if i.Code == 0 {
			errors++
			color.Red("%d %s %s\n", i.Code, i.Url, i.Dur.String())
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

func getter(getChan chan string, rChan chan urlCode, doneChan chan bool) {

	c := 0
	for url := range getChan {
		if Debug {
			c++
			color.Blue("Getting %s (%d)!", url, c)
		}
		s := time.Now()
		response, err := http.Get(url)
		d := time.Since(s)
		if err != nil {
			rChan <- urlCode{url, 0, d}
		} else {
			response.Body.Close()
			rChan <- urlCode{url, response.StatusCode, d}
		}

		if SleepTime > 0 {
			time.Sleep(SleepTime)
		}
	}

	doneChan <- true
}
