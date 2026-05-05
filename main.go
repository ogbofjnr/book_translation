package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	"golang.org/x/sync/errgroup"
)

const (
	deepseekURL           = "https://api.deepseek.com/chat/completions"
	defaultModel          = "deepseek-chat"
	defaultChunkRunes     = 1700
	defaultRequestTimeout = 90 * time.Second
	defaultPauseBetween   = 400 * time.Millisecond
	defaultTargetLanguage = "Russian"
	defaultCEFRThreshold  = "B2"
)

type deepseekRequest struct {
	Model       string            `json:"model"`
	Messages    []deepseekMessage `json:"messages"`
	Temperature float64           `json:"temperature"`
}

type deepseekMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type deepseekResponse struct {
	Choices []struct {
		Message deepseekMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type translator struct {
	apiKey      string
	model       string
	client      *http.Client
	chunkRunes  int
	maxChunks   int
	chunkIndex  int
	pause       time.Duration
	requestWait time.Duration
	debugDir    string
	epubOnly    string
	parallel    int
	targetLang  string
	cefrStart   string

	hadAPICallInCurrentHTML bool
	totalChunks             int32
	doneChunks              int32
}

func main() {
	_ = loadDotEnv(".env")

	inputFile := flag.String("input", "", "Process a single book file (overrides scanning input-dir)")
	inputDir := flag.String("input-dir", "books", "Directory with source books")
	outputDir := flag.String("output-dir", "books", "Directory for translated books when not using --in-place")
	inPlace := flag.Bool("in-place", true, "Overwrite input book in place")
	listOnly := flag.Bool("list", false, "List EPUB chapters in reading order with translation status and exit")
	apiKeyFlag := flag.String("api-key", "", "DeepSeek API key. If empty, DEEPSEEK_API_KEY is used")
	model := flag.String("model", defaultModel, "DeepSeek model name")
	chunkRunes := flag.Int("chunk-runes", defaultChunkRunes, "Max rune length for one API request")
	maxChunks := flag.Int("max-chunks", 0, "Limit processed chunks per file (0 = no limit)")
	timeout := flag.Duration("timeout", defaultRequestTimeout, "Timeout per API request")
	pause := flag.Duration("pause", defaultPauseBetween, "Pause between requests")
	debugDir := flag.String("debug-dir", "", "If set, dumps DeepSeek prompt/input/output per chunk into this directory")
	debugEnabled := flag.Bool("debug", false, "Enable writing DeepSeek debug dumps (requires --debug-dir)")
	epubOnly := flag.String("epub-only", "", "Translate only EPUB HTML files whose path contains this substring (case-insensitive)")
	parallel := flag.Int("parallel", 10, "Max concurrent DeepSeek requests per text node (1 = sequential). On any error, the book is not saved.")
	flag.Parse()

	if *listOnly {
		bookPath, err := resolveInputBook(*inputFile, *inputDir)
		if err != nil {
			log.Fatal(err)
		}
		if err := listEPUBChapters(bookPath); err != nil {
			log.Fatal(err)
		}
		return
	}

	apiKey := strings.TrimSpace(*apiKeyFlag)
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY"))
	}
	if apiKey == "" {
		log.Fatal("api key is empty: pass --api-key or set DEEPSEEK_API_KEY")
	}

	if err := os.MkdirAll(*outputDir, 0o755); err != nil {
		log.Fatalf("create output dir: %v", err)
	}

	par := *parallel
	if par < 1 {
		par = 1
	}
	if par > 32 {
		par = 32
	}

	targetLang := strings.TrimSpace(os.Getenv("TARGET_LANGUAGE"))
	if targetLang == "" {
		targetLang = defaultTargetLanguage
	}
	cefrStart := strings.ToUpper(strings.TrimSpace(os.Getenv("TARGET_ENGLISH_LEVEL")))
	if cefrStart == "" {
		cefrStart = defaultCEFRThreshold
	}

	t := &translator{
		apiKey:      apiKey,
		model:       *model,
		client:      &http.Client{Timeout: *timeout},
		chunkRunes:  *chunkRunes,
		maxChunks:   *maxChunks,
		pause:       *pause,
		requestWait: *timeout,
		debugDir:    strings.TrimSpace(*debugDir),
		epubOnly:    strings.ToLower(strings.TrimSpace(*epubOnly)),
		parallel:    par,
		targetLang:  targetLang,
		cefrStart:   cefrStart,
	}

	if !*debugEnabled {
		t.debugDir = ""
	}
	if t.debugDir != "" {
		if err := os.MkdirAll(t.debugDir, 0o755); err != nil {
			log.Fatalf("create debug dir: %v", err)
		}
	}

	var candidates []string
	if strings.TrimSpace(*inputFile) != "" {
		p := filepath.Clean(*inputFile)
		ext := strings.ToLower(filepath.Ext(p))
		if ext != ".epub" && ext != ".fb2" && ext != ".mobi" {
			log.Fatalf("unsupported input extension: %s", ext)
		}
		if _, err := os.Stat(p); err != nil {
			log.Fatalf("input file: %v", err)
		}
		candidates = append(candidates, p)
	} else {
		files, err := os.ReadDir(*inputDir)
		if err != nil {
			log.Fatalf("read input dir: %v", err)
		}

		for _, f := range files {
			if f.IsDir() {
				continue
			}
			ext := strings.ToLower(filepath.Ext(f.Name()))
			if ext == ".epub" || ext == ".fb2" || ext == ".mobi" {
				candidates = append(candidates, filepath.Join(*inputDir, f.Name()))
			}
		}
		sort.Strings(candidates)
	}
	if len(candidates) == 0 {
		log.Println("no .epub/.fb2/.mobi files found")
		return
	}

	for _, path := range candidates {
		t.chunkIndex = 0
		log.Printf("processing: %s", path)
		if err := processBook(path, *outputDir, *inPlace, t); err != nil {
			log.Printf("failed: %v", err)
			continue
		}
		log.Printf("done: %s", path)
	}
}

func resolveInputBook(inputFile, inputDir string) (string, error) {
	if strings.TrimSpace(inputFile) != "" {
		p := filepath.Clean(inputFile)
		ext := strings.ToLower(filepath.Ext(p))
		if ext != ".epub" {
			return "", fmt.Errorf("list mode supports only .epub files, got: %s", ext)
		}
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("input file: %w", err)
		}
		return p, nil
	}

	files, err := os.ReadDir(inputDir)
	if err != nil {
		return "", fmt.Errorf("read input dir: %w", err)
	}
	var candidates []string
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		if strings.EqualFold(filepath.Ext(f.Name()), ".epub") {
			candidates = append(candidates, filepath.Join(inputDir, f.Name()))
		}
	}
	sort.Strings(candidates)
	if len(candidates) == 0 {
		return "", fmt.Errorf("no .epub files found in %s", inputDir)
	}
	return candidates[0], nil
}

func listEPUBChapters(inputPath string) error {
	if !strings.EqualFold(filepath.Ext(inputPath), ".epub") {
		return fmt.Errorf("list mode supports only .epub files")
	}

	zr, err := zip.OpenReader(inputPath)
	if err != nil {
		return err
	}
	defer zr.Close()

	var opfPath string
	var opfFile *zip.File
	for _, f := range zr.File {
		if strings.HasSuffix(strings.ToLower(f.Name), ".opf") {
			opfPath = f.Name
			opfFile = f
			break
		}
	}
	if opfFile == nil {
		return fmt.Errorf("opf not found in %s", inputPath)
	}

	order := parseEPUBSpineOrder(opfFile)
	if len(order) == 0 {
		return fmt.Errorf("no chapters found in spine")
	}

	translated := loadTranslatedChapterSet(inputPath)
	opfDir := path.Dir(opfPath)

	fmt.Printf("Book: %s\n", filepath.Base(inputPath))
	fmt.Printf("Mode: in-place update\n\n")
	for i, rel := range order {
		chapterPath := epubNormZipPath(path.Clean(path.Join(opfDir, rel)))
		mark := "[ ]"
		if translated[chapterPath] {
			mark = "[x]"
		}
		fmt.Printf("%2d. %s %s\n", i+1, mark, chapterPath)
	}
	return nil
}

func loadTranslatedChapterSet(bookPath string) map[string]bool {
	out := map[string]bool{}
	if _, err := os.Stat(bookPath); err != nil {
		return out
	}

	zr, err := zip.OpenReader(bookPath)
	if err != nil {
		return out
	}
	defer zr.Close()

	for _, f := range zr.File {
		if !strings.HasSuffix(strings.ToLower(f.Name), ".opf") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return out
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return out
		}
		for _, p := range parseTranslatedChaptersMeta(string(data)) {
			out[epubNormZipPath(p)] = true
		}
		return out
	}
	return out
}

func loadDotEnv(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"'`)
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, val)
		}
	}
	return nil
}

func processBook(inputPath, outputDir string, inPlace bool, t *translator) error {
	ext := strings.ToLower(filepath.Ext(inputPath))
	outputPath := inputPath
	if !inPlace {
		baseNoExt := strings.TrimSuffix(filepath.Base(inputPath), ext)
		lowerBase := strings.ToLower(baseNoExt)
		stem := baseNoExt
		if strings.HasSuffix(lowerBase, "_translated") {
			stem = baseNoExt[:len(baseNoExt)-len("_translated")]
		}
		outputBase := stem + "_translated"
		outputPath = filepath.Join(outputDir, outputBase+ext)
	}

	switch ext {
	case ".fb2":
		return processFB2(inputPath, outputPath, t)
	case ".epub":
		return processEPUB(inputPath, outputPath, t)
	case ".mobi":
		return processMOBI(inputPath, outputPath, t)
	default:
		return fmt.Errorf("unsupported extension: %s", ext)
	}
}

func processFB2(inputPath, outputPath string, t *translator) error {
	raw, err := os.ReadFile(inputPath)
	if err != nil {
		return err
	}

	dec := xml.NewDecoder(bytes.NewReader(raw))
	var out bytes.Buffer
	enc := xml.NewEncoder(&out)
	enc.Indent("", "  ")

	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}

		switch tk := tok.(type) {
		case xml.CharData:
			original := string([]byte(tk))
			trimmed := strings.TrimSpace(original)
			if trimmed == "" {
				if err := enc.EncodeToken(tk); err != nil {
					return err
				}
				continue
			}
			translated, err := t.translateText(original)
			if err != nil {
				return err
			}
			if err := enc.EncodeToken(xml.CharData([]byte(translated))); err != nil {
				return err
			}
		default:
			if err := enc.EncodeToken(tok); err != nil {
				return err
			}
		}
	}
	if err := enc.Flush(); err != nil {
		return err
	}

	return writeFileAtomic(outputPath, out.Bytes(), 0o644)
}

func processEPUB(inputPath, outputPath string, t *translator) error {
	reader, err := zip.OpenReader(inputPath)
	if err != nil {
		return err
	}
	defer reader.Close()

	estimatedChunks, err := estimateEPUBChunks(reader.File, t)
	if err != nil {
		return fmt.Errorf("estimate epub chunks: %w", err)
	}
	if estimatedChunks > 0 {
		log.Printf("estimated chunks to process: %d", estimatedChunks)
	} else {
		log.Printf("estimated chunks to process: 0")
	}
	atomic.StoreInt32(&t.totalChunks, int32(estimatedChunks))
	atomic.StoreInt32(&t.doneChunks, 0)

	tmp, err := os.CreateTemp(filepath.Dir(outputPath), ".epub-out-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	cleanupOldEPUBTemps(filepath.Dir(outputPath), tmpPath)

	outFile, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	w := zip.NewWriter(outFile)
	success := false
	defer func() {
		if success {
			return
		}
		_ = w.Close()
		_ = outFile.Close()
		_ = os.Remove(tmpPath)
	}()

	var translatedHTML []string
	fileSnapshot := make(map[string][]byte)
	var opfHeader *zip.FileHeader
	var opfContent []byte

	for _, f := range orderedEPUBFiles(reader.File) {
		rc, err := f.Open()
		if err != nil {
			return err
		}
		content, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return err
		}

		modified := content
		nameLower := strings.ToLower(f.Name)
		if (strings.HasSuffix(nameLower, ".xhtml") || strings.HasSuffix(nameLower, ".html") || strings.HasSuffix(nameLower, ".htm")) && !isServiceHTMLByName(nameLower) && t.allowEPUBFile(nameLower) {
			t.hadAPICallInCurrentHTML = false
			modified, err = processHTMLContent(content, t)
			if err != nil {
				return fmt.Errorf("epub html %s: %w", f.Name, err)
			}
			if t.hadAPICallInCurrentHTML {
				translatedHTML = append(translatedHTML, f.Name)
			}
		}

		if strings.HasSuffix(nameLower, ".opf") {
			h := f.FileHeader
			opfHeader = &h
			opfContent = modified
			continue
		}

		fileSnapshot[epubNormZipPath(f.Name)] = modified

		h := f.FileHeader
		writer, err := w.CreateHeader(&h)
		if err != nil {
			return err
		}
		if _, err := writer.Write(modified); err != nil {
			return err
		}
	}

	if opfHeader != nil {
		verified := verifiedTranslatedChapterPaths(string(opfContent), translatedHTML, fileSnapshot)
		annotated := annotateOPFTranslationMetadata(opfContent, verified)
		writer, err := w.CreateHeader(opfHeader)
		if err != nil {
			return err
		}
		if _, err := writer.Write(annotated); err != nil {
			return err
		}
	}

	if err := w.Close(); err != nil {
		return err
	}
	if err := outFile.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, outputPath); err != nil {
		return err
	}
	success = true
	return nil
}

func cleanupOldEPUBTemps(dir, keepPath string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	keepAbs, err := filepath.Abs(keepPath)
	if err != nil {
		keepAbs = keepPath
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, ".epub-out-") {
			continue
		}
		p := filepath.Join(dir, name)
		pAbs, err := filepath.Abs(p)
		if err != nil {
			pAbs = p
		}
		if pAbs == keepAbs {
			continue
		}
		_ = os.Remove(p)
	}
}

func estimateEPUBChunks(files []*zip.File, t *translator) (int, error) {
	total := 0
	for _, f := range orderedEPUBFiles(files) {
		nameLower := strings.ToLower(f.Name)
		if !(strings.HasSuffix(nameLower, ".xhtml") || strings.HasSuffix(nameLower, ".html") || strings.HasSuffix(nameLower, ".htm")) {
			continue
		}
		if isServiceHTMLByName(nameLower) || !t.allowEPUBFile(nameLower) {
			continue
		}

		rc, err := f.Open()
		if err != nil {
			return 0, err
		}
		content, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return 0, err
		}

		n, err := estimateHTMLChunks(content, t.chunkRunes)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", f.Name, err)
		}
		total += n
	}

	if t.maxChunks > 0 {
		remaining := t.maxChunks - t.chunkIndex
		if remaining < 0 {
			remaining = 0
		}
		if total > remaining {
			total = remaining
		}
	}

	return total, nil
}

func estimateHTMLChunks(content []byte, chunkRunes int) (int, error) {
	doc, err := html.Parse(bytes.NewReader(content))
	if err != nil {
		return 0, err
	}

	body := findFirstElement(doc, atom.Body)
	if body == nil {
		return 0, nil
	}

	count := 0
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n == nil {
			return
		}
		if n.Type == html.ElementNode && shouldSkipElement(n) {
			return
		}
		if n.Type == html.TextNode {
			raw := n.Data
			if strings.TrimSpace(raw) != "" && shouldTranslateTextNode(n) {
				for _, s := range splitByRunes(raw, chunkRunes) {
					core, _, _ := splitEdgeWhitespace(s)
					if strings.TrimSpace(core) != "" {
						count++
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(body)
	return count, nil
}

func processMOBI(inputPath, outputPath string, t *translator) error {
	tmpDir, err := os.MkdirTemp("", "book-translate-mobi-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	epubTemp := filepath.Join(tmpDir, "in.epub")
	if err := convertEbook(inputPath, epubTemp); err != nil {
		return fmt.Errorf("convert mobi to epub: %w", err)
	}

	translatedEPUB := filepath.Join(tmpDir, "translated.epub")
	if err := processEPUB(epubTemp, translatedEPUB, t); err != nil {
		return err
	}

	if err := convertEbook(translatedEPUB, outputPath); err != nil {
		return fmt.Errorf("convert epub to mobi: %w", err)
	}

	return nil
}

func convertEbook(inputPath, outputPath string) error {
	cmd := exec.Command("ebook-convert", inputPath, outputPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func processHTMLContent(content []byte, t *translator) ([]byte, error) {
	doc, err := html.Parse(bytes.NewReader(content))
	if err != nil {
		return nil, err
	}

	body := findFirstElement(doc, atom.Body)
	if body == nil {
		var out bytes.Buffer
		if err := html.Render(&out, doc); err != nil {
			return nil, err
		}
		return out.Bytes(), nil
	}

	var walk func(*html.Node) error
	walk = func(n *html.Node) error {
		if n.Type == html.ElementNode && shouldSkipElement(n) {
			return nil
		}

		if n.Type == html.TextNode {
			raw := n.Data
			if strings.TrimSpace(raw) != "" && shouldTranslateTextNode(n) {
				translated, err := t.translateText(raw)
				if err != nil {
					return err
				}
				n.Data = translated
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if err := walk(c); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(body); err != nil {
		return nil, err
	}

	var out bytes.Buffer
	if err := html.Render(&out, doc); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func isServiceHTMLByName(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	base = strings.ReplaceAll(base, "-", "")
	base = strings.ReplaceAll(base, "_", "")
	return containsAny(base,
		"toc", "contents", "tableofcontents", "nav", "landmarks",
		"titlepage", "copyright", "colophon", "imprint",
		"index", "glossary", "aboutauthor", "cover",
	)
}

func (t *translator) allowEPUBFile(pathLower string) bool {
	if t.epubOnly == "" {
		return true
	}
	return strings.Contains(pathLower, t.epubOnly)
}

func findFirstElement(n *html.Node, a atom.Atom) *html.Node {
	if n == nil {
		return nil
	}
	if n.Type == html.ElementNode && n.DataAtom == a {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findFirstElement(c, a); found != nil {
			return found
		}
	}
	return nil
}

func shouldSkipElement(n *html.Node) bool {
	if n.DataAtom == atom.Script || n.DataAtom == atom.Style || n.DataAtom == atom.Nav {
		return true
	}

	for _, attr := range n.Attr {
		key := strings.ToLower(attr.Key)
		val := normalizeToken(attr.Val)
		if key == "epub:type" || key == "type" || key == "role" {
			if containsAny(val, "toc", "landmarks", "page-list", "titlepage", "copyright-page", "colophon", "index", "bibliography", "notes") {
				return true
			}
		}
		if key == "id" || key == "class" {
			if containsAny(val, "toc", "tableofcontents", "contents", "nav", "copyright", "colophon", "titlepage", "title-page", "index", "glossary", "imprint") {
				return true
			}
		}
	}
	return false
}

func shouldTranslateTextNode(n *html.Node) bool {
	if n == nil {
		return false
	}
	if ancestorMatches(n, atom.Head) || ancestorMatches(n, atom.Title) {
		return false
	}
	if ancestorHasAttrValue(n, "epub:type", "toc") {
		return false
	}
	txt := strings.TrimSpace(n.Data)
	if txt == "" {
		return false
	}
	if len([]rune(txt)) < 40 {
		return false
	}
	if mostlyNonLetters(txt) {
		return false
	}
	return true
}

func ancestorMatches(n *html.Node, a atom.Atom) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && p.DataAtom == a {
			return true
		}
	}
	return false
}

func ancestorHasAttrValue(n *html.Node, attrKey, expectedSubstring string) bool {
	attrKey = strings.ToLower(attrKey)
	expectedSubstring = strings.ToLower(expectedSubstring)
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type != html.ElementNode {
			continue
		}
		for _, attr := range p.Attr {
			if strings.ToLower(attr.Key) == attrKey && strings.Contains(strings.ToLower(attr.Val), expectedSubstring) {
				return true
			}
		}
	}
	return false
}

func containsAny(haystack string, needles ...string) bool {
	lh := strings.ToLower(haystack)
	for _, n := range needles {
		if strings.Contains(lh, strings.ToLower(n)) {
			return true
		}
	}
	return false
}

func normalizeToken(s string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "-", ""))
}

func mostlyNonLetters(s string) bool {
	letters := 0
	total := 0
	for _, r := range s {
		if unicode.IsSpace(r) {
			continue
		}
		total++
		if unicode.IsLetter(r) {
			letters++
		}
	}
	if total == 0 {
		return true
	}
	return letters*100/total < 45
}

func (t *translator) translateText(input string) (string, error) {
	segments := splitByRunes(input, t.chunkRunes)
	if len(segments) == 0 {
		return input, nil
	}

	type chunkJob struct {
		segIdx   int
		chunkNum int
		core     string
		leadWS   string
		tailWS   string
	}

	outs := make([]string, len(segments))
	var jobs []chunkJob

	for i, s := range segments {
		if t.maxChunks > 0 && t.chunkIndex >= t.maxChunks {
			outs[i] = s
			continue
		}
		t.chunkIndex++
		chunkNum := t.chunkIndex

		core, leadWS, tailWS := splitEdgeWhitespace(s)
		if strings.TrimSpace(core) == "" {
			outs[i] = s
			continue
		}
		jobs = append(jobs, chunkJob{
			segIdx: i, chunkNum: chunkNum, core: core, leadWS: leadWS, tailWS: tailWS,
		})
	}

	if len(jobs) == 0 {
		return strings.Join(outs, ""), nil
	}

	if t.parallel <= 1 {
		for _, j := range jobs {
			total := atomic.LoadInt32(&t.totalChunks)
			if total > 0 {
				log.Printf("translating chunk %d out of %d", j.chunkNum, total)
			} else {
				log.Printf("translating chunk %d", j.chunkNum)
			}
			translatedCore, err := t.translateChunk(j.core, j.chunkNum)
			if err != nil {
				return "", err
			}
			t.hadAPICallInCurrentHTML = true
			outs[j.segIdx] = j.leadWS + translatedCore + j.tailWS
			doneNow := atomic.AddInt32(&t.doneChunks, 1)
			if total > 0 {
				log.Printf("chunk done %d out of %d", doneNow, total)
			} else {
				log.Printf("chunk done %d", doneNow)
			}
			time.Sleep(t.pause)
		}
		return strings.Join(outs, ""), nil
	}

	sem := make(chan struct{}, t.parallel)
	var mu sync.Mutex
	g, _ := errgroup.WithContext(context.Background())
	for _, j := range jobs {
		j := j
		g.Go(func() error {
			sem <- struct{}{}
			defer func() { <-sem }()

			total := atomic.LoadInt32(&t.totalChunks)
			if total > 0 {
				log.Printf("translating chunk %d out of %d", j.chunkNum, total)
			} else {
				log.Printf("translating chunk %d (parallel)", j.chunkNum)
			}
			translatedCore, err := t.translateChunk(j.core, j.chunkNum)
			if err != nil {
				return err
			}
			mu.Lock()
			t.hadAPICallInCurrentHTML = true
			outs[j.segIdx] = j.leadWS + translatedCore + j.tailWS
			mu.Unlock()
			doneNow := atomic.AddInt32(&t.doneChunks, 1)
			if total > 0 {
				log.Printf("chunk done %d out of %d", doneNow, total)
			} else {
				log.Printf("chunk done %d", doneNow)
			}
			time.Sleep(t.pause)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return "", err
	}
	return strings.Join(outs, ""), nil
}

func (t *translator) translateChunk(text string, chunkNumber int) (string, error) {
	systemPrompt := fmt.Sprintf(`You are a careful literary editor.
Task: keep the original English text exactly, and only add %s translations in parentheses for words/phrases that are ABOVE CEFR %s level.
Rules:
1) Preserve all original words, punctuation, line breaks, spacing, and order.
2) Do NOT rewrite or paraphrase sentences.
3) Insert translations only where needed by context, as: difficult_word (one best %s meaning by context).
4) In each pair of parentheses, provide exactly one %s meaning (no synonyms, no alternatives, no comma-separated variants).
5) The translation must be literary and context-aware: choose the meaning that fits the whole sentence tone and sense, not a literal dictionary gloss.
6) Prefer natural %s wording that a human literary translator would choose in this context.
7) The %s meaning must be grammatically compatible with the local phrase/sentence role (part of speech, number, case, and natural collocation).
8) Avoid bare lemma-style glosses if a context-shaped form is needed.
9) Keep names and simple %s-or-lower words untouched.
10) Before finalizing, run a second pass over ALL inserted translations and verify each chosen %s meaning is the best fit for its exact local context; if not, replace it with the correct one.
11) Output ONLY the transformed text, with no explanations.`,
		t.targetLang, t.cefrStart, t.targetLang, t.targetLang, t.targetLang, t.targetLang, t.cefrStart, t.targetLang)

	userPrompt := "Process this text:\n\n" + text
	t.dumpDebugChunk(chunkNumber, "system_prompt.txt", systemPrompt)
	t.dumpDebugChunk(chunkNumber, "user_prompt.txt", userPrompt)
	t.dumpDebugChunk(chunkNumber, "input_text.txt", text)

	reqBody := deepseekRequest{
		Model: t.model,
		Messages: []deepseekMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		Temperature: 0.1,
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	var lastErr error
	for attempt := 1; attempt <= 4; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), t.requestWait)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, deepseekURL, bytes.NewReader(data))
		if err != nil {
			cancel()
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+t.apiKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err := t.client.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}

		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		if readErr != nil {
			lastErr = readErr
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}

		if resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("deepseek status %d: %s", resp.StatusCode, string(body))
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}

		var parsed deepseekResponse
		if err := json.Unmarshal(body, &parsed); err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}

		if parsed.Error != nil {
			lastErr = fmt.Errorf("deepseek error: %s", parsed.Error.Message)
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}
		if len(parsed.Choices) == 0 {
			lastErr = fmt.Errorf("deepseek empty choices")
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}

		out := parsed.Choices[0].Message.Content
		if strings.TrimSpace(out) == "" {
			lastErr = fmt.Errorf("deepseek empty content")
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}
		out = enforceSingleMeaning(out)
		t.dumpDebugChunk(chunkNumber, "output_text.txt", out)
		return out, nil
	}

	return "", fmt.Errorf("deepseek request failed: %w", lastErr)
}

func orderedEPUBFiles(files []*zip.File) []*zip.File {
	fileByName := make(map[string]*zip.File, len(files))
	for _, f := range files {
		fileByName[f.Name] = f
	}

	opfPath := ""
	for _, f := range files {
		if strings.HasSuffix(strings.ToLower(f.Name), ".opf") {
			opfPath = f.Name
			break
		}
	}
	if opfPath == "" {
		return files
	}

	order := parseEPUBSpineOrder(fileByName[opfPath])
	if len(order) == 0 {
		return files
	}

	opfDir := filepath.Dir(opfPath)
	used := make(map[string]bool, len(files))
	out := make([]*zip.File, 0, len(files))

	for _, rel := range order {
		full := filepath.Clean(filepath.Join(opfDir, rel))
		if f, ok := fileByName[full]; ok {
			out = append(out, f)
			used[full] = true
		}
	}
	for _, f := range files {
		if !used[f.Name] {
			out = append(out, f)
		}
	}
	return out
}

func parseEPUBSpineOrder(opfFile *zip.File) []string {
	if opfFile == nil {
		return nil
	}
	rc, err := opfFile.Open()
	if err != nil {
		return nil
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil
	}

	type item struct {
		ID   string `xml:"id,attr"`
		Href string `xml:"href,attr"`
	}
	type itemRef struct {
		IDRef string `xml:"idref,attr"`
	}
	type pkg struct {
		Manifest []item    `xml:"manifest>item"`
		Spine    []itemRef `xml:"spine>itemref"`
	}
	var p pkg
	if err := xml.Unmarshal(data, &p); err != nil {
		return nil
	}

	manifest := make(map[string]string, len(p.Manifest))
	for _, it := range p.Manifest {
		manifest[it.ID] = it.Href
	}
	var order []string
	for _, it := range p.Spine {
		if href, ok := manifest[it.IDRef]; ok && href != "" {
			order = append(order, href)
		}
	}
	return order
}

func enforceSingleMeaning(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, sz := utf8.DecodeRuneInString(s[i:])
		if r != '(' {
			b.WriteRune(r)
			i += sz
			continue
		}

		j := i + sz
		depth := 1
		for j < len(s) {
			r2, sz2 := utf8.DecodeRuneInString(s[j:])
			j += sz2
			if r2 == '(' {
				depth++
			} else if r2 == ')' {
				depth--
				if depth == 0 {
					break
				}
			}
		}
		if depth != 0 {
			b.WriteString(s[i:])
			break
		}

		content := s[i+sz : j-1]
		if containsCyrillic(content) {
			content = trimToSingleMeaning(content)
		}
		b.WriteRune('(')
		b.WriteString(content)
		b.WriteRune(')')
		i = j
	}
	return b.String()
}

func containsCyrillic(s string) bool {
	for _, r := range s {
		if (r >= 'А' && r <= 'я') || r == 'Ё' || r == 'ё' {
			return true
		}
	}
	return false
}

func trimToSingleMeaning(s string) string {
	low := strings.ToLower(s)
	if idx := strings.Index(low, " или "); idx >= 0 {
		return strings.TrimSpace(s[:idx])
	}
	for i, r := range s {
		if r == ',' || r == ';' || r == '/' {
			return strings.TrimSpace(s[:i])
		}
	}
	return strings.TrimSpace(s)
}

func annotateOPFTranslationMetadata(opf []byte, translatedFiles []string) []byte {
	if len(opf) == 0 {
		return opf
	}
	clean := dedupeSortedEPUBPaths(translatedFiles)

	chapters := strings.Join(clean, ";")
	ts := time.Now().UTC().Format(time.RFC3339)

	s := string(opf)
	reMeta := regexp.MustCompile(`(?s)\n?\s*<meta\s+name="x-translated-chapters"[^>]*>\s*</meta>|\n?\s*<meta\s+name="x-translated-chapters"[^>]*/>`)
	reMetaTS := regexp.MustCompile(`(?s)\n?\s*<meta\s+name="x-translation-updated-at"[^>]*>\s*</meta>|\n?\s*<meta\s+name="x-translation-updated-at"[^>]*/>`)
	s = reMeta.ReplaceAllString(s, "")
	s = reMetaTS.ReplaceAllString(s, "")

	insert := "\n    <meta name=\"x-translated-chapters\" content=\"" + xmlEscape(chapters) + "\"/>\n" +
		"    <meta name=\"x-translation-updated-at\" content=\"" + xmlEscape(ts) + "\"/>"

	lower := strings.ToLower(s)
	idx := strings.Index(lower, "</metadata>")
	if idx >= 0 {
		s = s[:idx] + insert + "\n  " + s[idx:]
		return []byte(s)
	}
	return opf
}

func epubNormZipPath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.ReplaceAll(p, "\\", "/")
	p = path.Clean("/" + p)
	if len(p) > 0 && p[0] == '/' {
		p = p[1:]
	}
	return p
}

func dedupeSortedEPUBPaths(paths []string) []string {
	seen := map[string]bool{}
	clean := make([]string, 0, len(paths))
	for _, p := range paths {
		pp := epubNormZipPath(p)
		if pp == "" || seen[pp] {
			continue
		}
		seen[pp] = true
		clean = append(clean, pp)
	}
	sort.Strings(clean)
	return clean
}

func verifiedTranslatedChapterPaths(opf string, apiTouched []string, fileSnapshot map[string][]byte) []string {
	candidates := dedupeSortedEPUBPaths(append(parseTranslatedChaptersMeta(opf), apiTouched...))
	verified := make([]string, 0, len(candidates))
	seen := map[string]bool{}
	for _, p := range candidates {
		key := epubNormZipPath(p)
		data, ok := fileSnapshot[key]
		if !ok {
			continue
		}
		if !containsCyrillicBytes(data) {
			continue
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		verified = append(verified, key)
	}
	sort.Strings(verified)
	return verified
}

func containsCyrillicBytes(b []byte) bool {
	return containsCyrillic(string(b))
}

func parseTranslatedChaptersMeta(opf string) []string {
	re := regexp.MustCompile(`<meta\s+[^>]*name\s*=\s*["']x-translated-chapters["'][^>]*>`)
	m := re.FindString(opf)
	if m == "" {
		return nil
	}
	sub := regexp.MustCompile(`content\s*=\s*["']([^"']*)["']`).FindStringSubmatch(m)
	if len(sub) < 2 {
		return nil
	}
	parts := strings.Split(sub[1], ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		pp := strings.TrimSpace(p)
		if pp != "" {
			out = append(out, pp)
		}
	}
	return out
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".write-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func xmlEscape(s string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"\"", "&quot;",
		"'", "&apos;",
	)
	return replacer.Replace(s)
}

func (t *translator) dumpDebugChunk(chunkNumber int, fileName string, content string) {
	if t.debugDir == "" {
		return
	}
	dir := filepath.Join(t.debugDir, fmt.Sprintf("chunk_%04d", chunkNumber))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("debug dump mkdir failed: %v", err)
		return
	}
	p := filepath.Join(dir, fileName)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		log.Printf("debug dump write failed (%s): %v", p, err)
	}
}

func splitByRunes(s string, maxRunes int) []string {
	if maxRunes <= 0 {
		return []string{s}
	}

	r := []rune(s)
	if len(r) <= maxRunes {
		return []string{s}
	}

	var parts []string
	start := 0
	for start < len(r) {
		end := start + maxRunes
		if end >= len(r) {
			parts = append(parts, string(r[start:]))
			break
		}

		split := chooseSentenceBoundary(r, start, end)
		if split <= start {
			split = end
		}
		parts = append(parts, string(r[start:split]))
		start = split
	}

	return parts
}

func chooseSentenceBoundary(r []rune, start, preferredEnd int) int {
	if preferredEnd <= start || preferredEnd >= len(r) {
		return preferredEnd
	}

	// Prefer finishing at sentence punctuation ahead of the limit for better context.
	forwardLimit := preferredEnd + preferredEnd/3
	if forwardLimit >= len(r) {
		forwardLimit = len(r) - 1
	}
	for i := preferredEnd; i <= forwardLimit; i++ {
		if isSentenceEndRune(r[i]) {
			return i + 1
		}
	}

	// Fallback: nearest sentence break behind the preferred end.
	backwardFloor := start + (preferredEnd-start)/2
	for i := preferredEnd; i >= backwardFloor; i-- {
		if isSentenceEndRune(r[i]) {
			return i + 1
		}
	}

	// Last fallback: split on whitespace close to preferred end.
	for i := preferredEnd; i >= backwardFloor; i-- {
		if unicode.IsSpace(r[i]) {
			return i + 1
		}
	}
	return preferredEnd
}

func isSentenceEndRune(r rune) bool {
	return r == '.' || r == '!' || r == '?' || r == ';' || r == '\n'
}

func splitEdgeWhitespace(s string) (core, leading, trailing string) {
	r := []rune(s)
	i := 0
	for i < len(r) && unicode.IsSpace(r[i]) {
		i++
	}
	j := len(r)
	for j > i && unicode.IsSpace(r[j-1]) {
		j--
	}
	return string(r[i:j]), string(r[:i]), string(r[j:])
}
