package main

/*
	Wgetpipe takes a list of fully-qualified URLs over STDIN and Gets them,
	outputting the code, url and elapsed fetch time.

	scans stdin in a goro, spawning up to MAX getters at a time, which stream their
	responses over a channel back to main which formats the output
*/

import (
	"github.com/cheggaaa/pb/v3"
	"github.com/cognusion/go-humanity"
	"github.com/cognusion/go-rangetripper/v2"
	"github.com/cognusion/go-signalhandler"
	"github.com/fatih/color"
	"github.com/viki-org/dnscache"

	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"
)

// Global-ish vars.
var (
	MaxRequests   int           // maximum number of outstanding HTTP get requests allowed
	SleepTime     time.Duration // Duration to sleep between GETter spawns
	ErrOnly       bool          // Quiet unless 0 == Code >= 400
	NoColor       bool          // Disable colorizing
	NoDNSCache    bool          // Disable DNS caching
	Summary       bool          // Output final stats
	Save          bool          // Enable saving the file
	useBar        bool          // Use progress bar
	totalGuess    int           // Guesstimate of number of GETs (useful with -bar)
	debug         bool          // Enable debugging
	ResponseDebug bool          // Enable full response output if debug
	timeout       time.Duration // How long each GET request may take
	chunkString   string        // Enable GET using ranges (large files)
	chunkSize     int64         // Numeric version of the prior.

	Client = new(http.Client)

	OutFormat = log.Ldate | log.Ltime | log.Lshortfile
	DebugOut  = log.New(io.Discard, "[DEBUG] ", OutFormat)
)

type urlCode struct {
	URL  string
	Code int
	Size int64
	Dur  time.Duration
	Err  error
}

// written to exclusively by collate().
// Unsafe to read from until rChan is closed.
type stat struct {
	count      int
	error4s    int
	error5s    int
	errors     int
	mismatches int
}

func init() {
	flag.IntVar(&MaxRequests, "max", 5, "Maximium in-flight GET requests at a time")
	flag.BoolVar(&ErrOnly, "errorsonly", false, "Only output errors (HTTP Codes >= 400)")
	flag.BoolVar(&NoColor, "nocolor", false, "Don't colorize the output")
	flag.BoolVar(&Summary, "stats", false, "Output stats at the end")
	flag.DurationVar(&SleepTime, "sleep", 0, "Amount of time to sleep between spawning a GETter (e.g. 1ms, 10s)")
	flag.DurationVar(&timeout, "timeout", 0, "Amount of time to allow each GET request (e.g. 30s, 5m)")
	flag.BoolVar(&debug, "debug", false, "Enable debug output")
	flag.BoolVar(&ResponseDebug, "responsedebug", false, "Enable full response output if debugging is on")
	flag.BoolVar(&NoDNSCache, "nodnscache", false, "Disable DNS caching")
	flag.BoolVar(&useBar, "bar", false, "Use progress bar instead of printing lines, can still use -stats")
	flag.IntVar(&totalGuess, "guess", 0, "Rough guess of how many GETs will be coming for -bar to start at. It will adjust")
	flag.BoolVar(&Save, "save", false, "Save the content of the files. Into hostname/folders/file.ext files")
	flag.StringVar(&chunkString, "size", "", "Size of chunks to download (whole-numbers with suffixes of B,KB,MB,GB,PB)")
	flag.Parse()

	// Handle boring people
	if NoColor {
		color.NoColor = true
	}

	// Handle debug
	if debug {
		DebugOut = log.New(os.Stderr, "[DEBUG] ", OutFormat)
	}

	// Handle chunk string-to-byte conversion
	if chunkString != "" {
		var cerr error
		chunkSize, cerr = humanity.StringAsBytes(chunkString)
		if cerr != nil {
			fmt.Printf("Please use wholenumbers with suffixes of B,KB,MB,GB,PB")
			flag.Usage()
			os.Exit(1)
		}

		rt, _ := rangetripper.NewWithLoggers(10, nil, DebugOut)
		rt.SetChunkSize(chunkSize)
		rt.SetClient(http.DefaultClient)
		Client.Transport = rt
	}

	// Sets the default http client to use dnscache, because duh
	if !NoDNSCache {
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

	var (
		bar       *pb.ProgressBar
		rStats    stat
		wg        sync.WaitGroup
		gwg       sync.WaitGroup
		getChan   = make(chan string, MaxRequests*10) // Channel to stream URLs to get
		rChan     = make(chan urlCode)                // Channel to stream responses from the Gets
		abortChan = make(chan bool)                   // Channel to tell the getters to abort
	)

	// Set up the progress bar
	if useBar {
		color.Output = io.Discard // disable colorized output
		tmpl := `{{string . "prefix"}}{{counters . }} {{bar . }} {{percent . }} {{rtime . "ETA %s"}}{{string . "suffix"}}`
		bar = pb.ProgressBarTemplate(tmpl).New(totalGuess)
		bar.Start()
	}

	sigDone := signalhandler.Simple(func(os.Signal) { close(abortChan) })
	defer sigDone()

	// Spawn off the getters
	gwg.Add(MaxRequests)
	for range MaxRequests {
		go getter(getChan, rChan, abortChan, timeout, &gwg)
	}

	// Hire a collator to collate the results
	wg.Add(1)
	go collate(&rStats, abortChan, rChan, &wg, bar)

	// spawn off the scanner
	start := time.Now()
	wg.Add(1)
	go scanStdIn(getChan, abortChan, &wg, bar)

	gwg.Wait() // Wait for getters
	close(rChan)
	wg.Wait() // Wait for processors

	elapsed := time.Since(start)
	if useBar {
		bar.Finish()
	}

	if Summary {
		color.Output = os.Stdout // re-enable this
		e := color.RedString("%d", rStats.errors)
		e4 := color.YellowString("%d", rStats.error4s)
		e5 := color.RedString("%d", rStats.error5s)
		eX := color.RedString("%d", rStats.mismatches)
		fmt.Printf("\n\nGETs: %d\nErrors: %s\n500 Errors: %s\n400 Errors: %s\nMismatches: %s\nElapsed Time: %s\n", rStats.count, e, e5, e4, eX, elapsed.String())
	}

}

func collate(stats *stat, abortChan chan bool, rChan chan urlCode, wg *sync.WaitGroup, bar *pb.ProgressBar) {
	defer DebugOut.Println("collate complete")
	defer wg.Done()

	var zu = urlCode{}
	for i := range rChan {
		if i == zu {
			// zero value means we're done
			return
		}

		// check for
		select {
		case <-abortChan:
			// Aborting
			DebugOut.Println("collate aborting!")
			return
		default:
		}

		stats.count++

		if bar != nil {
			bar.Increment()
		}

		if i.Code == 0 {
			stats.errors++
			color.Red("%d (%s) %s %s (%s)\n", i.Code, humanity.ByteFormat(i.Size), i.URL, i.Dur.String(), i.Err)
		} else if i.Code < 400 {
			if ErrOnly {
				// skip
				continue
			}
			color.Green("%d (%s) %s %s\n", i.Code, humanity.ByteFormat(i.Size), i.URL, i.Dur.String())
		} else if i.Code < 500 {
			stats.error4s++
			color.Yellow("%d (%s) %s %s\n", i.Code, humanity.ByteFormat(i.Size), i.URL, i.Dur.String())
		} else if i.Code < 600 {
			stats.error5s++
			color.Red("%d (%s) %s %s\n", i.Code, humanity.ByteFormat(i.Size), i.URL, i.Dur.String())
		} else {
			stats.mismatches++
			color.Red("%d (%s) %s %s\n", i.Code, humanity.ByteFormat(i.Size), i.URL, i.Dur.String())
		}
	}
}

// scanStdIn takes a channel to pass inputted strings to,
// and does so until EOF, whereafter it closes the channel
func scanStdIn(getChan chan string, abortChan chan bool, wg *sync.WaitGroup, bar *pb.ProgressBar) {
	defer close(getChan)
	defer wg.Done()

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
			if bar.Total() < count {
				bar.SetTotal(bar.Total() + 1)
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
func getter(getChan chan string, rChan chan urlCode, abortChan chan bool, timeout time.Duration, wg *sync.WaitGroup) {
	defer wg.Done()

	var (
		ctx    context.Context
		cancel context.CancelFunc
		abort  bool
	)

	go func() {
		// Wait until abort has been signalled,
		// then cancel all the things
		<-abortChan
		DebugOut.Println("getter abort seen!")
		abort = true
		if cancel != nil {
			cancel()
		}
	}()

	for url := range getChan {
		if abort {
			// Edge case: Abort has been called,
			// but we received a url via getChan
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

		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)

		s := time.Now()
		response, err := Client.Do(req)
		d := time.Since(s)

		if err != nil {
			// We assume code 0 to be a non-HTTP error
			DebugOut.Printf("Client.Do fetching '%s' returned error: %+v\n", url, err)
			rChan <- urlCode{url, 0, 0, d, err}
		} else {
			if ResponseDebug {
				b, err := io.ReadAll(response.Body)
				if err != nil {
					DebugOut.Printf("Error reading response body: %s\n", err)
				} else {
					DebugOut.Printf("<-----\n%s\n----->\n", b)
				}

				if Save {
					if err := SaveFile(url, &b); err != nil {
						fmt.Printf("Error saving response: '%s'\n", err)
					}
				}
			} else if Save {
				b, err := io.ReadAll(response.Body)
				if err != nil {
					fmt.Printf("Error reading response body: '%s' not saving file '%s'\n", err, url)
				} else {
					if err := SaveFile(url, &b); err != nil {
						fmt.Printf("Error saving response: '%s'\n", err)
					}
				}
			}
			rChan <- urlCode{url, response.StatusCode, response.ContentLength, d, nil}
			response.Body.Close() // else leak
		}
		cancel()

		if abort {
			DebugOut.Println("abort called, post")
			return
		}

		if SleepTime > 0 {
			// Zzzzzzz
			time.Sleep(SleepTime)
		}
	}

}

// SaveFile takes a URL and a pointer to a []byte containing the to-be-saved bytes,
// and saves the full url as the path (sans scheme).
// e.g. 'https://somewhere.com/1/2/3/4/5.html' will be saved as './somewhere.com/1/2/3/4/5.html'
func SaveFile(saveAs string, contents *[]byte) error {
	url, err := url.Parse(saveAs)
	if err != nil {
		return err
	}

	dirs := path.Dir(url.Path)
	if !strings.HasPrefix(dirs, "/") {
		// Sanity!
		dirs = "/" + dirs
	}

	DebugOut.Printf("Saved File Path: '%s%s' full: '%s%s'\n", url.Hostname(), dirs, url.Hostname(), url.Path)
	err = os.MkdirAll(fmt.Sprintf("%s%s", url.Hostname(), dirs), 0750)
	if err != nil {
		return err
	}

	err = os.WriteFile(fmt.Sprintf("%s%s", url.Hostname(), url.Path), *contents, 0600)
	if err != nil {
		return err
	}
	return nil
}
