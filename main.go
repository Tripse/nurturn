package main

import (
	"bufio"
	"embed"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// 使用 embed 将 config.yaml 嵌入到程序中
//
//go:embed config.yaml
var configFile embed.FS

// 配置结构体
type Config struct {
	Ext     []string `yaml:"ext"`
	Version []string `yaml:"version"`
}

// 扫描结果结构体
type Result struct {
	URL    string
	Status int
}

// 允许的状态码
var allowedStatus = map[int]bool{
	200: true, 201: true, 202: true, 203: true, 204: true, 205: true, 206: true,
	207: true, 208: true, 226: true, 301: true, 302: true, 403: true, 500: true,
}

// 自定义 HTTP 客户端，设置超时时间
var httpClient = &http.Client{
	Timeout: 10 * time.Second,
}

func main() {
	var urlStr string
	var file string
	var outputFile string
	flag.StringVar(&urlStr, "u", "", "指定单个URL")
	flag.StringVar(&file, "r", "", "指定结果文件路径")
	flag.StringVar(&outputFile, "o", "", "指定输出文件路径（可选）")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "用法: %s [选项]\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	// 参数校验
	if file == "" && urlStr == "" && !hasStdinData() {
		fmt.Println("必须指定 -u 或 -r 参数，或者通过管道传递数据")
		flag.Usage()
		os.Exit(1)
	}

	// 加载配置（从嵌入的文件中读取）
	config, err := loadConfigFromEmbed()
	if err != nil {
		fmt.Printf("配置加载失败: %v\n", err)
		os.Exit(1)
	}

	// 获取原始URL列表
	var urls []string
	if file != "" {
		urls, err = readUrlsFromFile(file)
		if err != nil {
			fmt.Printf("读取文件失败: %v\n", err)
			os.Exit(1)
		}
	} else if hasStdinData() {
		urls, err = readUrlsFromStdin()
		if err != nil {
			fmt.Printf("从标准输入读取失败: %v\n", err)
			os.Exit(1)
		}
	} else {
		urls = []string{urlStr}
	}

	if len(urls) == 0 {
		fmt.Println("无匹配对象")
		os.Exit(1)
	}

	// 提取端点和后缀，构建缓存字典
	var allTargets []string
	for _, urlStr := range urls {
		u, err := url.Parse(urlStr)
		if err != nil || u == nil {
			continue
		}

		// 提取路径中的所有端点
		endpoints, endpointCache := parseUrl(u.Path)
		if len(endpoints) == 0 {
			continue
		}

		// 生成目标 URL
		targets := generateTargets(u, endpoints, endpointCache, config)
		allTargets = append(allTargets, targets...)
	}

	// 去重后的总数
	var wg sync.WaitGroup
	results := make(chan Result, len(allTargets))
	resultSet := make(map[string]bool) // 用于去重

	// 发送请求
	for _, target := range allTargets {
		wg.Add(1)
		go func(target string) {
			defer wg.Done()
			resp, err := fetchURL(target, 3) // 增加重试机制
			if err != nil {
				return
			}
			defer resp.Body.Close()
			status := resp.StatusCode
			if allowedStatus[status] {
				results <- Result{URL: target, Status: status}
			}
		}(target)
	}

	// 等待完成并关闭通道
	go func() {
		wg.Wait()
		close(results)
	}()

	// 收集结果并去重
	var uniqueResults []Result
	for res := range results {
		if !resultSet[res.URL] {
			resultSet[res.URL] = true
			uniqueResults = append(uniqueResults, res)
		}
	}

	// 按 URL 排序（确保结果一致性）
	sort.Slice(uniqueResults, func(i, j int) bool {
		return uniqueResults[i].URL < uniqueResults[j].URL
	})

	// 输出结果
	if outputFile != "" {
		output, err := os.Create(outputFile)
		if err != nil {
			fmt.Printf("无法创建输出文件: %v\n", err)
			os.Exit(1)
		}
		defer output.Close()
		for _, res := range uniqueResults {
			writeToFile(output, res)
		}
	} else {
		for _, res := range uniqueResults {
			printResult(res)
		}
	}
}

// 从嵌入的文件中加载配置
func loadConfigFromEmbed() (*Config, error) {
	// 读取嵌入的 config.yaml 文件
	data, err := configFile.ReadFile("config.yaml")
	if err != nil {
		return nil, fmt.Errorf("无法读取嵌入的配置文件: %v", err)
	}

	// 解析 YAML 数据
	config := &Config{}
	if err := yaml.Unmarshal(data, config); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %v", err)
	}
	return config, nil
}

// 生成所有目标URL并去重
func generateTargets(baseURL *url.URL, endpoints []string, endpointCache map[string]string, config *Config) []string {
	var targets []string
	seen := make(map[string]bool) // 用于去重

	// 构建完整的路径前缀
	basePath := strings.TrimSuffix(baseURL.Path, "/")
	if !strings.HasSuffix(basePath, "/") {
		basePath += "/"
	}

	for i := range endpoints { // 遍历索引而不是值，避免未使用的变量
		// 获取当前层级的路径
		currentPath := strings.Join(endpoints[:i+1], "/")

		// 生成 ext payload
		for _, ext := range config.Ext {
			newURL := fmt.Sprintf("%s://%s/%s%s", baseURL.Scheme, baseURL.Host, currentPath, ext)
			if !seen[newURL] {
				seen[newURL] = true
				targets = append(targets, newURL)
			}
		}

		// 生成 version payload
		for _, ver := range config.Version {
			newURL := fmt.Sprintf("%s://%s/%s%s/", baseURL.Scheme, baseURL.Host, currentPath, ver)
			if !seen[newURL] {
				seen[newURL] = true
				targets = append(targets, newURL)
			}
		}

		// 如果是最后一部分，生成附加扩展名的 payload（如 /index.html.rar）
		if i == len(endpoints)-1 {
			lastPart := endpoints[i]
			if suffix, ok := endpointCache[lastPart]; ok {
				for _, ext := range config.Ext {
					newURL := fmt.Sprintf("%s://%s/%s%s%s", baseURL.Scheme, baseURL.Host, currentPath, suffix, ext)
					if !seen[newURL] {
						seen[newURL] = true
						targets = append(targets, newURL)
					}
				}
			}
		}
	}

	return targets
}

// 解析URL路径，提取所有端点和后缀
func parseUrl(path string) ([]string, map[string]string) {
	if path == "/" {
		return nil, nil
	}

	trimmedPath := strings.TrimPrefix(path, "/")
	parts := strings.Split(trimmedPath, "/")

	var endpoints []string
	endpointCache := make(map[string]string)

	for i, part := range parts {
		if part == "" {
			continue
		}

		// 如果是最后一部分，检查是否有扩展名
		if i == len(parts)-1 && strings.Contains(part, ".") {
			extIndex := strings.LastIndex(part, ".")
			endpoint := part[:extIndex]
			suffix := part[extIndex:]
			endpoints = append(endpoints, endpoint)
			endpointCache[endpoint] = suffix
		} else {
			// 否则直接将路径段视为端点
			endpoints = append(endpoints, part)
			endpointCache[part] = ""
		}
	}

	return endpoints, endpointCache
}

// 读取文件内容（添加正则提取URL）
func readUrlsFromFile(filename string) ([]string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var urls []string
	scanner := bufio.NewScanner(file)
	re := regexp.MustCompile(`https?://[^\s]+`) // 改进正则以匹配 http/https URL

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		matches := re.FindStringSubmatch(line)
		if len(matches) > 0 {
			urlStr := matches[0]
			urls = append(urls, urlStr)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return urls, nil
}

// 从标准输入读取 URL 列表
func readUrlsFromStdin() ([]string, error) {
	var urls []string
	scanner := bufio.NewScanner(os.Stdin)
	re := regexp.MustCompile(`https?://[^\s]+`) // 匹配 http/https URL

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		matches := re.FindStringSubmatch(line)
		if len(matches) > 0 {
			urlStr := matches[0]
			urls = append(urls, urlStr)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return urls, nil
}

// 输出彩色结果
func printResult(res Result) {
	var color string
	switch res.Status {
	case 200:
		color = "\033[32m" // 绿色
	case 302:
		color = "\033[33m" // 黄色
	case 500:
		color = "\033[31m" // 红色
	case 403:
		color = "\033[38;5;94m" // 棕色
	default:
		color = "\033[0m" // 默认颜色
	}

	fmt.Printf("%s%-6d\033[0m %s\n", color, res.Status, res.URL)
}

// 写入文件
func writeToFile(file *os.File, res Result) {
	line := fmt.Sprintf("%-6d %s\n", res.Status, res.URL)
	if _, err := file.WriteString(line); err != nil {
		fmt.Printf("写入文件时出错: %v\n", err)
	}
}

// 增加重试机制
func fetchURL(target string, retries int) (*http.Response, error) {
	var resp *http.Response
	var err error

	for i := 0; i < retries; i++ {
		resp, err = httpClient.Get(target)
		if err == nil {
			return resp, nil
		}
		time.Sleep(500 * time.Millisecond) // 等待 500 毫秒后重试
	}

	return nil, fmt.Errorf("failed after %d retries", retries)
}

// 检查是否有 stdin 数据
func hasStdinData() bool {
	stat, _ := os.Stdin.Stat()
	return (stat.Mode() & os.ModeCharDevice) == 0
}
