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
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

var (
	MAX        int           // maximum number of outstanding HTTP get requests allowed
	SleepTime  time.Duration // Duration to sleep between GETter spawns
	ErrOnly    bool          // Quiet unless 0 == Code >= 400
	NoColor    bool          // Disable colorizing
	NoDnsCache bool          // Disable DNS caching
	Summary    bool          // Output final stats
	debug      bool          // Enable debugging

	OutFormat int         = log.Ldate | log.Ltime | log.Lshortfile
	DebugOut  *log.Logger = log.New(ioutil.Discard, "[DEBUG] ", OutFormat)
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
	flag.BoolVar(&debug, "debug", false, "Enable debug output")
	flag.BoolVar(&NoDnsCache, "nodnscache", false, "Disable DNS caching")
	flag.Parse()

	// Handle boring people
	if NoColor {
		color.NoColor = true
	}

	// Handle debug
	if debug {
		DebugOut = log.New(os.Stderr, "[DEBUG] ", OutFormat)
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

	getChan := make(chan string)        // Channel to stream URLs to get
	rChan := make(chan urlCode)         // Channel to stream responses from the Gets
	doneChan := make(chan bool)         // Channel to signal a getter is done
	sigChan := make(chan os.Signal, 1)  // Channel to stream signals
	abortChan := make(chan bool, MAX+1) // Channel to tell the getters to abort
	count := 0
	error4s := 0
	error5s := 0
	errors := 0

	// Stream the signals we care about
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Signal handler
	go func() {
		<-sigChan
		DebugOut.Println("Signal seen, sending abort!")

		// Add an abort for each of the getters, plus the setter
		for a := 0; a <= MAX; a++ {
			abortChan <- true
		}
	}()

	// Spawn off the getters
	for g := 0; g < MAX; g++ {
		go getter(getChan, rChan, doneChan, abortChan)
	}

	// Block until all the getters are done, and then close rChan
	go func() {
		defer close(rChan)

		for c := 0; c < MAX; c++ {
			<-doneChan
			DebugOut.Printf("Done %d/%d\n", c+1, MAX)
		}
	}()

	// spawn off the scanner
	start := time.Now()
	go scanStdIn(getChan, abortChan)

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
func scanStdIn(getChan chan string, abortChan chan bool) {
	defer close(getChan)

	scanner := bufio.NewScanner(os.Stdin)
SCAN:
	for scanner.Scan() {
		select {
		case <-abortChan:
			DebugOut.Println("scanner abort seen!")
			break SCAN
		default:
		}
		DebugOut.Println("scanner sending...")

		getChan <- scanner.Text()

	}
	// POST: we've seen EOF
	DebugOut.Println("EOF seen")

}

// getter takes a receive channel, send channel, and done channel,
// running HTTP GETs for anything in the receive channel, returning
// formatted responses to the send channel, and signalling completion
// via the done channel
func getter(getChan chan string, rChan chan urlCode, doneChan chan bool, abortChan chan bool) {
	defer func() { doneChan <- true }()

	for {
		select {
		case <-abortChan:
			DebugOut.Println("getter abort seen!")
			return
		case url := <-getChan:
			if url == "" {
				// We assume an empty request is a closer
				// as that simplifies our for{select{}} loop
				// considerably
				DebugOut.Println("getter empty request seen!")
				return
			}
			DebugOut.Printf("getter getting %s\n", url)

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
}
