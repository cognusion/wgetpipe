package main

/*
	Wgetpipe takes a list of fully-qualified URLs over STDIN and Gets them,
	outputting the code, url and elapsed fetch time.

	scans stdin in a goro, spawning up to MAX getters at a time, which stream their
	responses over a channel back to main which formats the output
*/

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"github.com/cheggaaa/pb"
	"github.com/fatih/color"
	"github.com/viki-org/dnscache"
	"golang.org/x/net/context/ctxhttp"
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
	MAX           int           // maximum number of outstanding HTTP get requests allowed
	SleepTime     time.Duration // Duration to sleep between GETter spawns
	ErrOnly       bool          // Quiet unless 0 == Code >= 400
	NoColor       bool          // Disable colorizing
	NoDnsCache    bool          // Disable DNS caching
	Summary       bool          // Output final stats
	useBar        bool          // Use progress bar
	totalGuess    int           // Guesstimate of number of GETs (useful with -bar)
	debug         bool          // Enable debugging
	ResponseDebug bool          // Enable full response output if debug
	timeout       time.Duration // How long each GET request may take

	OutFormat int         = log.Ldate | log.Ltime | log.Lshortfile
	DebugOut  *log.Logger = log.New(ioutil.Discard, "[DEBUG] ", OutFormat)
)

type urlCode struct {
	Url  string
	Code int
	Size int64
	Dur  time.Duration
	Err  error
}

func init() {
	flag.IntVar(&MAX, "max", 5, "Maximium in-flight GET requests at a time")
	flag.BoolVar(&ErrOnly, "errorsonly", false, "Only output errors (HTTP Codes >= 400)")
	flag.BoolVar(&NoColor, "nocolor", false, "Don't colorize the output")
	flag.BoolVar(&Summary, "stats", false, "Output stats at the end")
	flag.DurationVar(&SleepTime, "sleep", 0, "Amount of time to sleep between spawning a GETter (e.g. 1ms, 10s)")
	flag.DurationVar(&timeout, "timeout", 0, "Amount of time to allow each GET request (e.g. 30s, 5m)")
	flag.BoolVar(&debug, "debug", false, "Enable debug output")
	flag.BoolVar(&ResponseDebug, "responsedebug", false, "Enable full response output if debugging is on")
	flag.BoolVar(&NoDnsCache, "nodnscache", false, "Disable DNS caching")
	flag.BoolVar(&useBar, "bar", false, "Use progress bar instead of printing lines, can still use -stats")
	flag.IntVar(&totalGuess, "guess", 0, "Rough guess of how many GETs will be coming for -bar to start at. It will adjust")
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

	var bar *pb.ProgressBar
	getChan := make(chan string, MAX*10) // Channel to stream URLs to get
	rChan := make(chan urlCode)          // Channel to stream responses from the Gets
	doneChan := make(chan bool)          // Channel to signal a getter is done
	sigChan := make(chan os.Signal, 1)   // Channel to stream signals
	abortChan := make(chan bool)         // Channel to tell the getters to abort
	count := 0
	error4s := 0
	error5s := 0
	errors := 0

	// Set up the progress bar
	if useBar {
		bar = pb.New(totalGuess)
	}

	// Stream the signals we care about
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Signal handler
	go func() {
		<-sigChan
		DebugOut.Println("Signal seen, sending abort!")

		close(abortChan)
	}()

	// Spawn off the getters
	for g := 0; g < MAX; g++ {
		go getter(getChan, rChan, doneChan, abortChan, timeout)
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
	go scanStdIn(getChan, abortChan, bar)

	if useBar {
		bar.Start()
	}
	// Collate the results
	for i := range rChan {
		count++

		if useBar {
			bar.Increment()
		}
		if i.Code == 0 {
			errors++
			if useBar {
				continue
			}
			color.Red("%d (%s) %s %s (%s)\n", i.Code, ByteFormat(i.Size), i.Url, i.Dur.String(), i.Err)
		} else if i.Code < 400 {
			if ErrOnly || useBar {
				// skip
				continue
			}
			color.Green("%d (%s) %s %s\n", i.Code, ByteFormat(i.Size), i.Url, i.Dur.String())
		} else if i.Code < 500 {
			error4s++
			if useBar {
				continue
			}
			color.Yellow("%d (%s) %s %s\n", i.Code, ByteFormat(i.Size), i.Url, i.Dur.String())
		} else {
			error5s++
			if useBar {
				continue
			}
			color.Red("%d (%s) %s %s\n", i.Code, ByteFormat(i.Size), i.Url, i.Dur.String())
		}
	}

	if useBar {
		bar.Finish()
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
func scanStdIn(getChan chan string, abortChan chan bool, bar *pb.ProgressBar) {
	defer close(getChan)

	scanner := bufio.NewScanner(os.Stdin)
	count := int64(0)
	for scanner.Scan() {
		select {
		case <-abortChan:
			DebugOut.Println("scanner abort seen!")
			return
		default:
		}
		DebugOut.Println("scanner sending...")

		getChan <- scanner.Text()
		count++
		if bar != nil {
			if bar.Total < count {
				bar.Total += 1
			}
		}

	}
	// POST: we've seen EOF
	DebugOut.Printf("EOF seen after %d lines\n", count)

}

// getter takes a receive channel, send channel, and done channel,
// running HTTP GETs for anything in the receive channel, returning
// formatted responses to the send channel, and signalling completion
// via the done channel
func getter(getChan chan string, rChan chan urlCode, doneChan chan bool, abortChan chan bool, timeout time.Duration) {
	defer func() { doneChan <- true }()

	var (
		ctx    context.Context
		cancel context.CancelFunc
		abort  bool
	)
	c := &http.Client{}

	go func() {
		<-abortChan
		DebugOut.Println("getter abort seen!")
		abort = true
		if cancel != nil {
			cancel()
		}
		return
	}()

	for url := range getChan {
		if abort {
			DebugOut.Println("abort called")
			return
		} else if url == "" {
			// We assume an empty request is a closer
			// as that simplifies our for{select{}} loop
			// considerably
			DebugOut.Println("getter empty request seen!")
			return
		}
		DebugOut.Printf("getter getting %s\n", url)

		// Create the context
		if timeout > 0 {
			ctx, cancel = context.WithTimeout(context.Background(), timeout)
		} else {
			ctx, cancel = context.WithCancel(context.Background())
		}

		// GET!
		s := time.Now()
		response, err := ctxhttp.Get(ctx, c, url)
		d := time.Since(s)
		cancel()

		if err != nil {
			// We assume code 0 to be a non-HTTP error
			rChan <- urlCode{url, 0, 0, d, err}
		} else {
			if ResponseDebug {
				b, err := ioutil.ReadAll(response.Body)
				if err != nil {
					DebugOut.Printf("Error reading response body: %s\n", err)
				} else {
					DebugOut.Printf("<-----\n%s\n----->\n", b)
				}
			}
			response.Body.Close() // else leak
			rChan <- urlCode{url, response.StatusCode, response.ContentLength, d, nil}
		}

		if SleepTime > 0 {
			// Zzzzzzz
			time.Sleep(SleepTime)
		}
	}

}

// ByteFormat returns a human-readable string representation of a byte count
func ByteFormat(num_in int64) string {
	suffix := "B"
	num := float64(num_in)
	units := []string{"", "K", "M", "G", "T", "P", "E", "Z"} // "Y" caught  below
	for _, unit := range units {
		if num < 1024.0 {
			return fmt.Sprintf("%3.1f%s%s", num, unit, suffix)
		}
		num = (num / 1024)
	}
	return fmt.Sprintf("%.1f%s%s", num, "Y", suffix)
}
