package main

import (
	"bufio"
	"context"
	"runtime"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 自定义类型用于处理多个文件名参数
type stringSlice []string

func (s *stringSlice) String() string {
	return fmt.Sprintf("%v", *s)
}

func (s *stringSlice) Set(value string) error {
	*s = append(*s, value)
	return nil
}

var (
	filenameout = flag.String("filenameout", "有效数据", "输出文件名前缀")
	filenames   stringSlice
	testurl     = flag.String("testurl", "testurl", "测试URL文件")
	speedmin    = flag.Float64("speedmin", 5.0, "最低速度限制")
	ipnumsave   = flag.Int("ipnumsave", 20, "保留IP数量")

	cnList = []string{"szx", "pvg", "zhy", "bjs"}
)

type IPData struct {
	IP           string `json:"ip"`
	Port         string `json:"port"`
	UseTime      int    `json:"use_time,omitempty"`
	XAmzCfPop    string `json:"x_amz_cf_pop,omitempty"`
	Speed        string `json:"speed,omitempty"`
	AsOrganization string `json:"as_organization,omitempty"`
}

func main() {
	flag.Var(&filenames, "filenames", "输入文件名列表")
	flag.Parse()

	// 读取测试URL
	testURLs := readTestURLs(*testurl + ".txt")

	// 读取并处理数据
	uniqueIPs := processInputFiles(filenames)

	// 执行延迟测试
	successfulIPs := latencyTest(uniqueIPs)

	// 执行速度测试
	speedTest(successfulIPs, testURLs)

	// 写入最终结果
	writeFinalResults(successfulIPs)
}

func readTestURLs(filename string) []string {
	file, err := os.Open(filename)
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()

	var urls []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		urls = append(urls, strings.TrimSpace(scanner.Text()))
	}
	return urls
}

func processInputFiles(filenames []string) []*IPData {
	var allIPs []*IPData
	seen := make(map[string]bool)

	for _, fname := range filenames {
		file, err := os.Open(fname + ".json")
		if err != nil {
			log.Printf("无法打开文件 %s: %v", fname, err)
			continue
		}

		decoder := json.NewDecoder(file)
		for {
			var data IPData
			if err := decoder.Decode(&data); err == io.EOF {
				break
			} else if err != nil {
				log.Printf("JSON解析错误: %v", err)
				continue
			}

			if !seen[data.IP] {
				seen[data.IP] = true
				allIPs = append(allIPs, &data)
			}
		}
		file.Close()
	}
	return allIPs
}

func latencyTest(ips []*IPData) []*IPData {
	var (
		wg         sync.WaitGroup
		mu         sync.Mutex
		results    []*IPData
		client     = &http.Client{Timeout: 5 * time.Second}
		total      = len(ips)
		processed  int32
		progressMu sync.Mutex
	)

	sem := make(chan struct{}, getCPUCount())

	// 进度打印协程
	go func() {
		for {
			time.Sleep(1 * time.Second)
			current := atomic.LoadInt32(&processed)
			progressMu.Lock()
			fmt.Printf("\r[Latency Test] 进度：%d/%d (%.1f%%)", 
				current, total, float64(current)/float64(total)*100)
			progressMu.Unlock()
			if int(current) >= total {
				break
			}
		}
	}()

	for _, ip := range ips {
		wg.Add(1)
		sem <- struct{}{}

		go func(ip *IPData) {
			defer func() {
				<-sem
				wg.Done()
				atomic.AddInt32(&processed, 1)
			}()

			url := fmt.Sprintf("http://%s:%s", ip.IP, ip.Port)
			start := time.Now()

			resp, err := client.Get(url)
			if err != nil {
				return
			}
			defer resp.Body.Close()

			server := resp.Header.Get("Server")
			pop := resp.Header.Get("x-amz-cf-pop")

			if server == "CloudFront" {
				useTime := int(time.Since(start).Milliseconds())

				mu.Lock()
				ip.UseTime = useTime
				ip.XAmzCfPop = pop
				results = append(results, ip)
				mu.Unlock()
			}
		}(ip)
	}

	wg.Wait()
	fmt.Println("\n延迟测试完成")

	// 排序并过滤中国节点
	filtered := filterAndSort(results)
	
	// 保存中间结果
	saveJSON(filtered, *filenameout+"-out.json")
	
	return filtered[:int(math.Min(float64(len(filtered)), float64(*ipnumsave)))]
}

func filterAndSort(ips []*IPData) []*IPData {
	var filtered []*IPData
	for _, ip := range ips {
		valid := true
		pop := strings.ToLower(ip.XAmzCfPop)
		for _, cn := range cnList {
			if strings.Contains(pop, cn) {
				valid = false
				break
			}
		}
		if valid {
			filtered = append(filtered, ip)
		}
	}

	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].UseTime < filtered[j].UseTime
	})

	return filtered
}

func speedTest(ips []*IPData, testURLs []string) {
	var (
		wg          sync.WaitGroup
		mu          sync.Mutex
		results     []*IPData
		ipRegex     = regexp.MustCompile(`\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}`)
		maxWorkers  = getCPUCount()
		successChan = make(chan *IPData, *ipnumsave)
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	workQueue := make(chan *IPData, len(ips))
	for _, ip := range ips {
		workQueue <- ip
	}
	close(workQueue)

	for i := 0; i < maxWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ip := range workQueue {
				select {
				case <-ctx.Done():
					return
				default:
					cmd := exec.Command("./CloudflareST",
						"-ip", ip.IP,
						"-tp", ip.Port,
						"-url", testURLs[rand.Intn(len(testURLs))],
						"-tl", "5000",
						"-dn", "20",
						"-p", "20")

					output, _ := cmd.CombinedOutput()
					lines := strings.Split(string(output), "\n")
					for _, line := range lines {
						if ipRegex.MatchString(line) {
							fields := strings.Fields(line)
							if len(fields) >= 6 {
								speed, _ := strconv.ParseFloat(fields[5], 64)
								if speed >= *speedmin {
									mu.Lock()
									ip.Speed = fields[5]
									results = append(results, ip)
									// 输出测速结果
									log.Printf("[Speed] 发现有效IP %s:%s 速度 %.1fMB/s 延迟 %dms(已收集 %d/%d)",
										ip.IP, ip.Port, speed, ip.UseTime, len(results), *ipnumsave)
									if len(results) >= *ipnumsave {
										cancel()
									}
									mu.Unlock()
									successChan <- ip
								}
							}
						}
					}
				}
			}
		}()
	}

	wg.Wait()
	close(successChan)

	// 更新结果集并排序
	sort.Slice(results, func(i, j int) bool {
		s1, _ := strconv.ParseFloat(results[i].Speed, 64)
		s2, _ := strconv.ParseFloat(results[j].Speed, 64)
		return s1 > s2
	})

	// 保留前N个结果
	if len(results) > *ipnumsave {
		results = results[:*ipnumsave]
	}
}

func writeFinalResults(ips []*IPData) {
	file, err := os.Create(*filenameout + ".txt")
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()

	for _, ip := range ips {
		// 处理组织信息
		org := strings.TrimSpace(ip.AsOrganization)
		if org == "" {
			org = "other"
		} else {
			switch {
			case strings.Contains(strings.ToLower(org), "alibaba"):
				org = "ali"
			case strings.Contains(strings.ToLower(org), "tencent"):
				org = "tx"
			case strings.Contains(strings.ToLower(org), "china mobile"):
				org = "cm"
			case strings.Contains(strings.ToLower(org), "china telecom"):
				org = "ct"
			}
		}

		line := fmt.Sprintf("%s:%s#%s_%s_%dms_%sMB\n",
			ip.IP, ip.Port, org, ip.XAmzCfPop, ip.UseTime, ip.Speed)
		file.WriteString(line)
	}
}

func getCPUCount() int {
	if n := runtime.NumCPU(); n > 0 {
		return n
	}
	return 4
}

func saveJSON(data []*IPData, filename string) {
	file, err := os.Create(filename)
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	for _, d := range data {
		encoder.Encode(d)
	}
}
