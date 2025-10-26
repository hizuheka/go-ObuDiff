package main

import (
	"bufio"
	"encoding/csv"
	"flag"
	"fmt"
	"html"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/sergi/go-diff/diffmatchpatch"
)

// diffRegex は [-old-]{+new+} の形式をキャプチャします。
var diffRegex = regexp.MustCompile(`^\s*\[\-(.*?)\-\]\{\+(.*?)\+\}\s*$`)

// dmpPool は *diffmatchpatch.DiffMatchPatch オブジェクトをプールします
var dmpPool = sync.Pool{
	New: func() interface{} {
		return diffmatchpatch.New()
	},
}

// Config はフラグの値を保持する構造体
type Config struct {
	InputPath  string // 空の場合は stdin を示す
	OutputPath string
	FormatHTML bool
	LightMode  bool
	LineLimit  int
	FontFamily string
	Headers    []string
}

func main() {
	// 1. フラグをパース
	inputPath := flag.String("i", "", "入力CSVファイルパス (省略した場合は標準入力から読み込み)")
	outputPath := flag.String("o", "", "出力ファイルパス (必須)")
	formatHTML := flag.Bool("html", false, "HTML形式で出力する")
	lightMode := flag.Bool("light", false, "軽量リスト形式(差分のみ)で出力します (デフォルトは全データ形式)")
	lineLimit := flag.Int("n", 0, "処理する最大行数を指定します (0の場合は全行を処理)")
	defaultFontStack := `"Helvetica Neue", Arial, "Hiragino Kaku Gothic ProN", "Hiragino Sans", Meiryo, sans-serif`
	fontFamily := flag.String("font", defaultFontStack, "HTML出力時に使用するCSSのfont-familyを指定します")
	headerStr := flag.String("header", "", "CSVのヘッダー行をカンマ区切りで指定します")

	flag.Parse()

	// 2. slog ロガーをセットアップ
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// 3. -o のみチェック
	if *outputPath == "" {
		logger.Error("エラー: -o (出力パス) は必須です。")
		flag.Usage()
		os.Exit(1)
	}

	// 4. -header を csv.Reader で堅牢にパース
	var headers []string
	if *headerStr != "" {
		r := csv.NewReader(strings.NewReader(*headerStr))
		var err error
		headers, err = r.Read()
		if err != nil {
			logger.Error("-header の解析に失敗しました", "error", err)
			os.Exit(1)
		}
	}

	// 5. Config 構造体に格納
	cfg := Config{
		InputPath:  *inputPath,
		OutputPath: *outputPath,
		FormatHTML: *formatHTML,
		LightMode:  *lightMode,
		LineLimit:  *lineLimit,
		FontFamily: *fontFamily,
		Headers:    headers,
	}

	// 6. 入力ストリームのセットアップ
	var inStream io.ReadCloser
	var err error

	if cfg.InputPath == "" {
		inStream = os.Stdin
		logger.Info("標準入力から読み込みます...")
	} else {
		inStream, err = os.Open(cfg.InputPath)
		if err != nil {
			logger.Error("入力ファイルを開けません", "path", cfg.InputPath, "error", err)
			os.Exit(1)
		}
		logger.Info("入力ファイルから読み込みます", "path", cfg.InputPath)
	}
	defer inStream.Close()

	// 7. 出力ファイルのセットアップ
	outFile, err := os.Create(cfg.OutputPath)
	if err != nil {
		logger.Error("出力ファイルを作成できません", "path", cfg.OutputPath, "error", err)
		os.Exit(1)
	}
	defer outFile.Close()

	// 8. Reader/Writer を作成
	reader := csv.NewReader(inStream)
	reader.ReuseRecord = true
	writer := bufio.NewWriter(outFile)
	defer writer.Flush()

	// 9. ロジック本体を呼び出す
	dmp := dmpPool.Get().(*diffmatchpatch.DiffMatchPatch)
	defer dmpPool.Put(dmp)

	if err := executeProcessing(cfg, reader, writer, dmp, logger); err != nil {
		logger.Error("処理中にエラーが発生しました", "error", err)
		os.Exit(1)
	}

	// 10. 完了メッセージ
	if cfg.LineLimit > 0 {
		fmt.Printf("先頭 %d 行の差分ハイライト処理が完了しました: %s\n", cfg.LineLimit, cfg.OutputPath)
	} else {
		fmt.Printf("差分ハイライト処理が完了しました: %s\n", cfg.OutputPath)
	}
}

// executeProcessing は、I/O(Reader/Writer)と設定(Config)を引数にとる、テスト可能な関数
func executeProcessing(cfg Config, reader *csv.Reader, writer io.Writer, dmp *diffmatchpatch.DiffMatchPatch, logger *slog.Logger) error {
	if cfg.LightMode {
		// --- 軽量リストモード (差分のみ) ---
		csvWriter := csv.NewWriter(writer)

		if cfg.FormatHTML {
			logger.Info("HTML形式 (軽量リスト) で処理を開始します...")
			return processHTMLAsList(reader, writer, dmp, cfg.FontFamily, cfg.LineLimit, cfg.Headers)
		}
		// 軽量 CSV リスト
		logger.Info("CSV形式 (軽量リスト) で処理を開始します...")
		err := processCSVAsList(reader, csvWriter, dmp, cfg.LineLimit, cfg.Headers)
		csvWriter.Flush() // csvWriterのバッファを io.Writer に書き出す
		return err

	}
	// --- 全データモード (デフォルト) ---
	csvWriter := csv.NewWriter(writer)

	if cfg.FormatHTML {
		logger.Info("HTML形式 (全データテーブル) で処理を開始します...")
		return processHTMLAsTable(reader, writer, dmp, cfg.FontFamily, cfg.LineLimit, cfg.Headers)
	}
	// 全データ CSV
	logger.Info("CSV形式 (全データ) で処理を開始します...")
	err := processCSVAsFull(reader, csvWriter, dmp, cfg.LineLimit, cfg.Headers)
	csvWriter.Flush() // csvWriterのバッファを io.Writer に書き出す
	return err
}

// processCSVAsFull は、全データをCSV形式で出力します。
func processCSVAsFull(reader *csv.Reader, writer *csv.Writer, dmp *diffmatchpatch.DiffMatchPatch, lineLimit int, headers []string) error {
	var lineCount int
	if headers != nil {
		if err := writer.Write(headers); err != nil {
			return fmt.Errorf("CSVヘッダーの書き込みに失敗: %w", err)
		}
	}

	for {
		if lineLimit > 0 && lineCount >= lineLimit {
			break
		}
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("CSV行の読み取りに失敗 (line %d): %w", lineCount+1, err)
		}
		lineCount++

		outputRecord := make([]string, len(record))
		for i, cell := range record {
			diffs, isDiff := parseDiffCell(cell, dmp)
			if isDiff {
				outputRecord[i] = formatDiffsToText(diffs)
			} else {
				outputRecord[i] = cell
			}
		}

		if err := writer.Write(outputRecord); err != nil {
			return fmt.Errorf("CSV行の書き込みに失敗 (line %d): %w", lineCount, err)
		}
	}
	return nil
}

// processCSVAsList は、差分のみを CSV (行,列,値) 形式で出力します。
func processCSVAsList(reader *csv.Reader, writer *csv.Writer, dmp *diffmatchpatch.DiffMatchPatch, lineLimit int, headers []string) error {
	var lineCount int
	if err := writer.Write([]string{"Line", "Column", "DiffValue"}); err != nil {
		return fmt.Errorf("軽量CSVヘッダーの書き込みに失敗: %w", err)
	}

	for {
		if lineLimit > 0 && lineCount >= lineLimit {
			break
		}
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("CSV行の読み取りに失敗 (line %d): %w", lineCount+1, err)
		}
		lineCount++

		for colNum, cell := range record { // colNum は 0-based
			diffs, isDiff := parseDiffCell(cell, dmp)
			if isDiff {
				diffText := formatDiffsToText(diffs)
				colStr := fmt.Sprintf("%d", colNum+1)
				if headers != nil && colNum < len(headers) {
					colStr = fmt.Sprintf("%d:%s", colNum+1, headers[colNum])
				}
				row := []string{
					fmt.Sprintf("%d", lineCount), // Line
					colStr,                       // Column
					diffText,                     // DiffValue
				}
				if err := writer.Write(row); err != nil {
					return fmt.Errorf("軽量CSV行の書き込みに失敗 (line %d): %w", lineCount, err)
				}
			}
		}
	}
	return nil
}

// processHTMLAsList は、差分があった箇所のみをリスト形式で書き出します。
func processHTMLAsList(reader *csv.Reader, writer io.Writer, dmp *diffmatchpatch.DiffMatchPatch, fontFamily string, lineLimit int, headers []string) error {
	var err error
	write := func(s string) {
		if err != nil {
			return
		}
		_, err = io.WriteString(writer, s)
	}
	writeHTMLHeaderList(writer, fontFamily)

	var lineCount int
	var diffFoundCount int

	for {
		if lineLimit > 0 && lineCount >= lineLimit {
			break
		}
		record, readErr := reader.Read()
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("CSV行の読み取りに失敗 (line %d): %w", lineCount+1, readErr)
		}
		lineCount++

		for colNum, cell := range record {
			diffs, isDiff := parseDiffCell(cell, dmp)
			if isDiff {
				diffFoundCount++
				htmlDiff := formatDiffsToHTML(diffs)
				writeHTMLDiffLine(writer, lineCount, colNum+1, htmlDiff, headers)
			}
		}
	}

	if diffFoundCount == 0 {
		write("<p class='no-diff'>差分は見つかりませんでした。</p>\n")
	}

	writeHTMLFooterList(writer)
	return err
}

// processHTMLAsTable は、全データをテーブル形式で書き出します。
func processHTMLAsTable(reader *csv.Reader, writer io.Writer, dmp *diffmatchpatch.DiffMatchPatch, fontFamily string, lineLimit int, headers []string) error {
	writeHTMLHeaderTable(writer, fontFamily, headers)

	io.WriteString(writer, "<tbody>\n")
	var lineCount int

	for {
		if lineLimit > 0 && lineCount >= lineLimit {
			break
		}
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("CSV行の読み取りに失敗 (line %d): %w", lineCount+1, err)
		}
		lineCount++

		outputCells := make([]string, len(record))
		for i, cell := range record {
			diffs, isDiff := parseDiffCell(cell, dmp)
			if isDiff {
				outputCells[i] = formatDiffsToHTML(diffs)
			} else {
				outputCells[i] = html.EscapeString(cell)
			}
		}
		writeHTMLDataRowTable(writer, outputCells)
	}
	io.WriteString(writer, "</tbody>\n")
	writeHTMLFooterTable(writer)
	return nil
}

// --- セル処理関数 ---

func parseDiffCell(cell string, dmp *diffmatchpatch.DiffMatchPatch) ([]diffmatchpatch.Diff, bool) {
	matches := diffRegex.FindStringSubmatch(cell)
	if matches == nil {
		return nil, false
	}
	// [1] = oldVal, [2] = newVal
	oldVal := matches[1]
	newVal := matches[2]
	diffs := dmp.DiffMain(oldVal, newVal, false)
	dmp.DiffCleanupSemantic(diffs)
	return diffs, true
}

func formatDiffsToText(diffs []diffmatchpatch.Diff) string {
	var builder strings.Builder
	for _, diff := range diffs {
		switch diff.Type {
		case diffmatchpatch.DiffEqual:
			builder.WriteString(diff.Text)
		case diffmatchpatch.DiffDelete:
			fmt.Fprintf(&builder, "[-%.*s-]", len(diff.Text), diff.Text)
		case diffmatchpatch.DiffInsert:
			fmt.Fprintf(&builder, "{+%.*s+}", len(diff.Text), diff.Text)
		}
	}
	return builder.String()
}

func formatDiffsToHTML(diffs []diffmatchpatch.Diff) string {
	var builder strings.Builder
	for _, diff := range diffs {
		escapedText := html.EscapeString(diff.Text)
		switch diff.Type {
		case diffmatchpatch.DiffEqual:
			builder.WriteString(escapedText)
		case diffmatchpatch.DiffDelete:
			fmt.Fprintf(&builder, `<del class="diff-del">%.*s</del>`, len(escapedText), escapedText)
		case diffmatchpatch.DiffInsert:
			fmt.Fprintf(&builder, `<ins class="diff-add">%.*s</ins>`, len(escapedText), escapedText)
		}
	}
	return builder.String()
}

// --- HTMLヘルパー関数 ---

func writeHTMLHeaderList(w io.Writer, fontFamily string) {
	safeFontFamily := strings.ReplaceAll(fontFamily, "<", "")
	safeFontFamily = strings.ReplaceAll(safeFontFamily, ">", "")
	io.WriteString(w, `<!DOCTYPE html>
<html lang="ja">
<head>
    <meta charset="UTF-8">
    <title>差分比較結果 (不一致リスト)</title>
    <style>
`)
	fmt.Fprintf(w, "        body { font-family: %s; }\n", safeFontFamily)
	io.WriteString(w, `
        .diff-del { color: #d32f2f; text-decoration: line-through; background-color: #ffebee; }
        .diff-add { color: #388e3c; font-weight: bold; text-decoration: none; background-color: #e8f5e9; }
        .diff-line { padding: 8px 12px; border-bottom: 1px solid #eee; line-height: 1.5; background-color: #f9f9f9; }
        .diff-line:nth-child(even) { background-color: #fff; }
        .diff-line .location { font-weight: bold; color: #555; margin-right: 15px; display: inline-block; min-width: 150px; }
        .no-diff { font-size: 1.2em; color: #777; padding: 20px; }
    </style>
</head>
<body>
    <h1>差分比較結果 (不一致のみ)</h1>
`)
}

func writeHTMLDiffLine(w io.Writer, line, col int, htmlDiff string, headers []string) {
	io.WriteString(w, "<div class='diff-line'>\n")
	colStr := fmt.Sprintf("Col %d", col)
	if headers != nil && col-1 < len(headers) {
		colStr = fmt.Sprintf("Col %d:%s", col, html.EscapeString(headers[col-1]))
	}
	fmt.Fprintf(w, "    <span class='location'>(Line %d, %s)</span>\n", line, colStr)
	fmt.Fprintf(w, "    <span class='value'>%s</span>\n", htmlDiff)
	io.WriteString(w, "</div>\n")
}

func writeHTMLFooterList(w io.Writer) {
	io.WriteString(w, `</body>
</html>
`)
}

func writeHTMLHeaderTable(w io.Writer, fontFamily string, headers []string) {
	safeFontFamily := strings.ReplaceAll(fontFamily, "<", "")
	safeFontFamily = strings.ReplaceAll(safeFontFamily, ">", "")
	io.WriteString(w, `<!DOCTYPE html>
<html lang="ja">
<head>
    <meta charset="UTF-8">
    <title>差分比較結果 (全データ)</title>
    <style>
`)
	fmt.Fprintf(w, "        body { font-family: %s; }\n", safeFontFamily)
	io.WriteString(w, `
        .diff-del { color: #d32f2f; text-decoration: line-through; background-color: #ffebee; }
        .diff-add { color: #388e3c; font-weight: bold; text-decoration: none; background-color: #e8f5e9; }
        table { border-collapse: collapse; margin: 20px 0; font-size: 0.9em; }
        th, td { border: 1px solid #ccc; padding: 8px 12px; vertical-align: top; text-align: left; }
        thead th { background-color: #f0f0f0; }
        tbody tr:nth-child(odd) { background-color: #f9f9f9; }
    </style>
</head>
<body>
    <h1>差分比較結果 (全データ)</h1>
    <table>
`)
	if headers != nil {
		io.WriteString(w, "<thead>\n<tr>\n")
		for _, h := range headers {
			fmt.Fprintf(w, "    <th>%s</th>\n", html.EscapeString(h))
		}
		io.WriteString(w, "</tr>\n</thead>\n")
	}
}

func writeHTMLDataRowTable(w io.Writer, cells []string) {
	io.WriteString(w, "<tr>\n")
	for _, c := range cells {
		fmt.Fprintf(w, "    <td>%s</td>\n", c)
	}
	io.WriteString(w, "</tr>\n")
}

func writeHTMLFooterTable(w io.Writer) {
	io.WriteString(w, `</table>
</body>
</html>
`)
}
