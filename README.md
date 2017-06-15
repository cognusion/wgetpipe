# wgetpipe
Wgetpipe takes a list of fully-qualified URLs over STDIN and Gets them, outputting the code, url and elapsed fetch time. 

It scans STDIN, spawning up to _-max_ getters at a time, which stream their responses back to the collator to format the output. This tool was generated to aid in seeding pull-through caches, but has utility in othere areas as well

## Usage

```BASH
  -debug
    	Enable debug output
  -errorsonly
    	Only output errors (HTTP Codes >= 400)
  -max int
    	Maximium in-flight GET requests at a time (default 5)
  -nocolor
    	Don't colorize the output
  -nodnscache
    	Disable DNS caching
  -sleep duration
    	Amount of time to sleep between spawning a GETter (e.g. 1ms, 10s)
  -stats
    	Output stats at the end
```

## Licensing

MIT

Thank you to SpatialKey for allowing this tool to be released, as it was initially developed for them.

