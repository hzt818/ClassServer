// Copyright (c) 黄智韬. All rights reserved.

// 聊天室 AI 的本地 Bing 搜索（RAG）：服务器直接检索 Bing，把结果作为
// 参考上下文注入 @AI 的提问，让回答能引用实时资料并标注来源。
//
// 两种模式（search.bing_api_key 是否配置）：
//  1. 配置了 Bing Web Search API v7 密钥 → 走官方 API（稳定，付费）；
//  2. 未配置 → 免密抓取 Bing 网页结果页（默认 cn.bing.com，国内可达），
//     解析自然结果卡片；页面结构变化时可能失效，届时配置官方密钥即可。
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"visualclassroom/classserver/config"
)

// bingResult 一条搜索结果。
type bingResult struct {
	Title   string
	URL     string
	Snippet string
}

var bingHTTPClient = &http.Client{Timeout: 10 * time.Second}

func bingSearchEnabled() bool { return config.LoadConfig().BingEnabled }

func bingSearchCount() int {
	n := config.LoadConfig().BingCount
	if n <= 0 || n > 10 {
		return 5
	}
	return n
}

// bingWebSearch 检索 Bing：配置了官方密钥走 API，否则走免密网页抓取。
func bingWebSearch(ctx context.Context, query string, topK int) ([]bingResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("空查询")
	}
	if topK <= 0 {
		topK = 5
	}
	if key := config.LoadConfig().BingAPIKey; key != "" {
		return bingSearchAPI(ctx, query, topK, key)
	}
	return bingSearchScrape(ctx, query, topK)
}

// ---------- 模式一：Bing Web Search API v7 ----------

func bingSearchAPI(ctx context.Context, query string, topK int, key string) ([]bingResult, error) {
	u := "https://api.bing.microsoft.com/v7.0/search?q=" + url.QueryEscape(query) +
		"&count=" + strconv.Itoa(topK) + "&mkt=zh-CN&responseFilter=Webpages"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Ocp-Apim-Subscription-Key", key)
	req.Header.Set("Accept", "application/json")
	resp, err := bingHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Bing API %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var parsed struct {
		WebPages struct {
			Value []struct {
				Name    string `json:"name"`
				URL     string `json:"url"`
				Snippet string `json:"snippet"`
			} `json:"value"`
		} `json:"webPages"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	results := make([]bingResult, 0, len(parsed.WebPages.Value))
	for _, v := range parsed.WebPages.Value {
		if v.Name == "" || v.URL == "" {
			continue
		}
		results = append(results, bingResult{Title: v.Name, URL: v.URL, Snippet: v.Snippet})
		if len(results) >= topK {
			break
		}
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("Bing API 无结果")
	}
	return results, nil
}

// ---------- 模式二：免密网页抓取（cn.bing.com 结果页） ----------

var (
	bingBlockRe   = regexp.MustCompile(`(?s)<li class="b_algo".*?</li>`)
	bingLinkRe    = regexp.MustCompile(`(?s)<h2[^>]*>\s*<a[^>]+href="(http[^"]+)"[^>]*>(.*?)</a>`)
	bingSnipRe    = regexp.MustCompile(`(?s)<p[^>]*>(.*?)</p>`)
	htmlTagRe     = regexp.MustCompile(`<[^>]+>`)
	htmlEntityAmp = strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", "\"", "&#39;", "'", "&nbsp;", " ")
)

func stripHTML(s string) string {
	s = htmlTagRe.ReplaceAllString(s, "")
	return strings.TrimSpace(htmlEntityAmp.Replace(s))
}

func bingSearchScrape(ctx context.Context, query string, topK int) ([]bingResult, error) {
	u := "https://cn.bing.com/search?q=" + url.QueryEscape(query) +
		"&count=" + strconv.Itoa(topK) + "&setlang=zh-hans&mkt=zh-CN"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	// 常规浏览器头，降低被反爬拦截的概率
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	resp, err := bingHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Bing 网页 %d", resp.StatusCode)
	}
	html := string(body)
	results := make([]bingResult, 0, topK)
	for _, block := range bingBlockRe.FindAllString(html, topK*2) {
		m := bingLinkRe.FindStringSubmatch(block)
		if m == nil {
			continue
		}
		title := stripHTML(m[2])
		link := m[1]
		snippet := ""
		if sm := bingSnipRe.FindStringSubmatch(block); sm != nil {
			snippet = stripHTML(sm[1])
		}
		if title == "" || link == "" || !strings.HasPrefix(link, "http") {
			continue
		}
		results = append(results, bingResult{Title: title, URL: link, Snippet: snippet})
		if len(results) >= topK {
			break
		}
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("Bing 网页解析无结果（页面结构可能变化，可配置 search.bing_api_key 走官方 API）")
	}
	return results, nil
}

// injectBingContext 为聊天 AI 提问注入本地搜索结果；返回拼接后的内容与是否注入成功。
func injectBingContext(ctx context.Context, userContent string) (string, bool) {
	if !bingSearchEnabled() {
		return userContent, false
	}
	searchCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	results, err := bingWebSearch(searchCtx, userContent, bingSearchCount())
	if err != nil || len(results) == 0 {
		return userContent, false
	}
	var sb strings.Builder
	sb.WriteString("\n\n[本地 Bing 搜索参考结果]\n")
	for i, r := range results {
		sb.WriteString(fmt.Sprintf("%d. %s\n   %s\n   来源：%s\n", i+1, r.Title, r.Snippet, r.URL))
	}
	sb.WriteString("\n（回答时可参考以上搜索结果：引用了某条结果就在句末标注 [编号]，并在回答末尾另起一行以\"参考来源：\"列出对应链接；" +
		"若结果与问题无关，请忽略并按你自己的知识回答。）")
	return userContent + sb.String(), true
}
