package main

import (
  "context"
  "fmt"
  "log/slog"
  "net/http"
  "net/http/httptrace"
  "net/url"
  "os"
  "strings"
  "time"
  "crypto/tls"
  "gopkg.in/yaml.v3"
  "github.com/enriquebris/goconcurrentqueue"
)

type Check struct {
    Name string `yaml:"name"`
    Domain string
    URL string `yaml:"url"`
    Method string `yaml:"method"`
    Headers map[string]string `yaml:"headers"`
    Body string `yaml:"body"`
    IP string
}

type CheckResult struct {
    OriginalCheck Check
    Domain string
    Result bool
}

type DomainStats struct {
    DomainName string
    Up int
    Total int
}

func main() {

    var programLevel = new(slog.LevelVar)
    h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: programLevel})
    slog.SetDefault(slog.New(h))
    if len(os.Args) > 2 && os.Args[2] == "debug" {
        programLevel.Set(slog.LevelDebug)
    }

    //Load Checks Config
    var checks []Check = loadConfig(os.Args[1])
    
    var domainStats map[string]DomainStats = groupIntoDomains(checks)
    
    interval := time.Duration(15)*time.Second

    sendQueue := goconcurrentqueue.NewFIFO()
    resultsQueue := goconcurrentqueue.NewFIFO()
    
    go runChecks(sendQueue, resultsQueue)
    queueChecks(sendQueue, checks, interval)
    go handleResults(resultsQueue, domainStats, len(checks))

    //Loop until done.
    for{
        
    }

    fmt.Println("Done")
}

func loadConfig(configPath string)([]Check) {
    var checks []Check
    
    slog.Debug(fmt.Sprintf("Loading configuration: %s", configPath))

    yamlFile, err := os.ReadFile(configPath)
    if err != nil {
        slog.Error("Error opening config", err)
        panic(err)
    }

    err = yaml.Unmarshal(yamlFile, &checks)
    if err != nil {
        slog.Error("Error reading config", err)
        panic(err)
    }

    return checks
}

func queueChecks(sendQueue goconcurrentqueue.Queue, checks []Check, interval time.Duration) {
    ticker := time.NewTicker(interval)
    done := make(chan bool)

    go func() {
       for{
           select {
           case <-done:
               return
            case t := <-ticker.C:
                _ = t
                slog.Debug(fmt.Sprintf("Tick at", t))  
                for _,c := range checks {
                    sendQueue.Enqueue(c)
                }
           }
       } 
    }()
}

func groupIntoDomains(checks []Check)(map[string]DomainStats) {
    groups := make(map[string]DomainStats)
    for index,c := range checks{
        cDomain := getDomain(c.URL)
        c.Domain = cDomain
        checks[index] = c
        if _, ok := groups[cDomain]; !ok {
            groups[cDomain] = DomainStats{Up: 0, Total: 0, DomainName: cDomain}
        } 
    }
    return groups
}

func getDomain(fullurl string)(domain string) {
    url, err := url.Parse(fullurl)
    if err != nil {
        slog.Error("Error parsing url", err)
        panic(err)
    }
    parts := strings.Split(url.Hostname(), ".")
    domain = parts[len(parts)-2] + "." + parts[len(parts)-1]
    return domain
}

func runChecks(sendQueue, resultQueue goconcurrentqueue.Queue) {
    for {
        value, _ := sendQueue.DequeueOrWaitForNextElement()
        check := value.(Check)
        result := sendRequest(check)
        resultQueue.Enqueue(CheckResult{OriginalCheck: check, Result: result, Domain: check.Domain})
    }
}

func sendRequest(check Check)(status bool) {
    slog.Debug(fmt.Sprintf("Sending message: %s", check.Name))

    //Set timeout to 500ms
    ctx, cncl := context.WithTimeout(context.Background(), time.Millisecond*500)
    defer cncl()
    
    req, _ := http.NewRequestWithContext(ctx, check.Method, check.URL, nil)

    var start, connect, dns, tlsHandshake time.Time

    trace := &httptrace.ClientTrace{
        DNSStart: func(dsi httptrace.DNSStartInfo) { dns = time.Now() },
        DNSDone: func(ddi httptrace.DNSDoneInfo) {
            slog.Debug(fmt.Sprintf("DNS Done: %v", time.Since(dns)))
        },

        TLSHandshakeStart: func() { tlsHandshake = time.Now() },
        TLSHandshakeDone: func(cs tls.ConnectionState, err error) {
            slog.Debug(fmt.Sprintf("TLS Handshake: %v", time.Since(tlsHandshake)))
        },

        ConnectStart: func(network, addr string) { connect = time.Now() },
        ConnectDone: func(network, addr string, err error) {
            slog.Debug(fmt.Sprintf("Connect time: %v", time.Since(connect)))
        },

        GotFirstResponseByte: func() {
            slog.Debug(fmt.Sprintf("Time from start to first byte: %v", time.Since(start)))
        },
    }

    req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
    start = time.Now()

    resp, _ := http.DefaultTransport.RoundTrip(req)
    
    totalTime := time.Since(start)
    timeout := time.Duration(500)*time.Millisecond

    finalresult := false
    if resp.StatusCode == 200 && totalTime < timeout {
        finalresult = true
    }

    return finalresult
}

func handleResults(resultQueue goconcurrentqueue.Queue, stats map[string]DomainStats, round int) {
    for{
        for range round {
            value, _ := resultQueue.DequeueOrWaitForNextElement()
            result := value.(CheckResult)

            st := stats[result.Domain]
            st.Total++
            if result.Result {
                st.Up++
            }
            stats[result.Domain] = st
        }

        for domain,s := range stats {
            availability := 100.00* (float64(s.Up) / float64(s.Total))
            fmt.Printf("%v has %.0f%% availability percentage\n", domain, availability)
            slog.Debug(fmt.Sprintf("%v Up: %v Total: %v", domain, s.Up, s.Total))
        }
    }
}
