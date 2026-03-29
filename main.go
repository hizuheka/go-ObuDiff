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
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/transform"
)

// diffRegex は [-old-]{+new+} の形式をキャプチャします。
// 1. Change: [-old-]{+new+} OR {-old-}{+new+}
var diffRegexChange = regexp.MustCompile(`(?s)^\s*(?:\[\-(.*?)\-\]|\{\-(.*?)\-\})\s*\{\+(.*?)\+\}\s*$`)

// 2. Add: {+new+}
var diffRegexAdd = regexp.MustCompile(`(?s)^\s*\{\+(.*?)\+\}\s*$`)

// 3. Delete: [-old-] OR {-old-}
var diffRegexDel = regexp.MustCompile(`(?s)^\s*(?:\[\-(.*?)\-\]|\{\-(.*?)\-\})\s*$`)

// 行全体の差分判定用
var rowAddRegex = regexp.MustCompile(`^\s*\{\+(.*)\+\}\s*$`)
var rowDelRegex = regexp.MustCompile(`^\s*(?:\[\-(.*)\-\]|\{\-(.*)\-\})\s*$`)

// dmpPool は *diffmatchpatch.DiffMatchPatch オブジェクトをプールします
var dmpPool = sync.Pool{
	New: func() interface{} {
		return diffmatchpatch.New()
	},
}

// Config はフラグの値を保持する構造体
type Config struct {
	InputPath    string
	OutputPath   string
	FormatHTML   bool
	LightMode    bool
	EnableFilter bool
	TrimSpaces   bool
	UseCSVQuote  bool
	LineLimit    int
	FontFamily   string
	Headers      []string
	SjisInput    bool
	ExcelMode    bool
}

// RecordReader はCSVのようなレコード読み込みの抽象化インターフェースです
type RecordReader interface {
	Read() ([]string, error)
}

// SimpleCSVReader はクォートを考慮せず単純にカンマで区切るリーダーです
type SimpleCSVReader struct {
	scanner *bufio.Scanner
}

func NewSimpleCSVReader(r io.Reader) *SimpleCSVReader {
	return &SimpleCSVReader{scanner: bufio.NewScanner(r)}
}

func (r *SimpleCSVReader) Read() ([]string, error) {
	if !r.scanner.Scan() {
		err := r.scanner.Err()
		if err == nil {
			return nil, io.EOF
		}
		return nil, err
	}
	line := r.scanner.Text()

	// 1. 行全体が追加/削除マーカーで囲まれているかチェック
	var isRowAdd, isRowDel bool
	var content string

	if matches := rowAddRegex.FindStringSubmatch(line); matches != nil {
		isRowAdd = true
		content = matches[1]
	} else if matches := rowDelRegex.FindStringSubmatch(line); matches != nil {
		isRowDel = true
		content = matches[1]
		if content == "" {
			content = matches[2]
		}
	} else {
		content = line
	}

	// 2. カンマで分割
	fields := strings.Split(content, ",")

	// 3. 各フィールドの後処理
	for i, field := range fields {
		// クォート除去
		if len(field) >= 2 && strings.HasPrefix(field, "\"") && strings.HasSuffix(field, "\"") {
			fields[i] = field[1 : len(field)-1]
		}

		// 行全体が追加/削除だった場合、各セルにもマーカーを付与
		if isRowAdd {
			fields[i] = "{+" + fields[i] + "+}"
		} else if isRowDel {
			fields[i] = "[-" + fields[i] + "-]"
		}
	}
	return fields, nil
}

// removeBOM はUTF-8のBOMがあれば除去したReaderを返します
func removeBOM(r io.Reader) io.Reader {
	br := bufio.NewReader(r)
	r1, _, err := br.ReadRune()
	if err != nil {
		return br
	}
	if r1 != '\uFEFF' {
		br.UnreadRune()
	}
	return br
}

func main() {
	inputPath := flag.String("i", "", "入力CSVファイルパス (省略した場合は標準入力から読み込み)")
	outputPath := flag.String("o", "", "出力ファイルパス (必須)")
	formatHTML := flag.Bool("html", false, "HTML形式で出力する")
	lightMode := flag.Bool("light", false, "軽量リスト形式(差分のみ)で出力します (デフォルトは全データ形式)")
	enableFilter := flag.Bool("filter", false, "HTMLテーブル出力時にフィルタ機能(JavaScript)を追加します")
	trimSpaces := flag.Bool("trim", false, "差分がないセルの末尾の全角スペースのみを削除して表示幅を最適化します")
	useCSVQuote := flag.Bool("strict-csv", false, "CSVの厳密なクォート処理(\")を有効にします。指定しない場合、\"は単なる文字として扱われ、行単位で単純分割されます")
	lineLimit := flag.Int("n", 0, "処理する最大行数を指定します (0の場合は全行を処理)")
	defaultFontStack := `"Helvetica Neue", Arial, "Hiragino Kaku Gothic ProN", "Hiragino Sans", Meiryo, sans-serif`
	fontFamily := flag.String("font", defaultFontStack, "HTML出力時に使用するCSSのfont-familyを指定します")
	headerStr := flag.String("header", "", "CSVのヘッダー行をカンマ区切りで指定します")
	sjisInput := flag.Bool("sjis", false, "入力ファイルをShift_JISとして読み込みます（出力はUTF-8）")
	excelMode := flag.Bool("excel", false, "ExcelでHTMLを開く際に見やすくするための互換スタイル(<font>タグ等)を出力します")

	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if *outputPath == "" {
		logger.Error("エラー: -o (出力パス) は必須です。")
		flag.Usage()
		os.Exit(1)
	}

	var headers []string
	if *headerStr != "" {
		var r RecordReader
		if *useCSVQuote {
			csvR := csv.NewReader(strings.NewReader(*headerStr))
			csvR.LazyQuotes = true
			r = csvR
		} else {
			r = NewSimpleCSVReader(strings.NewReader(*headerStr))
		}
		var err error
		headers, err = r.Read()
		if err != nil {
			logger.Error("-header の解析に失敗しました", "error", err)
			os.Exit(1)
		}
	}

	cfg := Config{
		InputPath:    *inputPath,
		OutputPath:   *outputPath,
		FormatHTML:   *formatHTML,
		LightMode:    *lightMode,
		EnableFilter: *enableFilter,
		TrimSpaces:   *trimSpaces,
		UseCSVQuote:  *useCSVQuote,
		LineLimit:    *lineLimit,
		FontFamily:   *fontFamily,
		Headers:      headers,
		SjisInput:    *sjisInput,
		ExcelMode:    *excelMode,
	}

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

	var readerInput io.Reader = inStream
	if cfg.SjisInput {
		logger.Info("入力をShift_JISとしてデコードします")
		readerInput = transform.NewReader(inStream, japanese.ShiftJIS.NewDecoder())
	}

	bomFreeReader := removeBOM(readerInput)

	outFile, err := os.Create(cfg.OutputPath)
	if err != nil {
		logger.Error("出力ファイルを作成できません", "path", cfg.OutputPath, "error", err)
		os.Exit(1)
	}
	defer outFile.Close()

	var reader RecordReader
	if cfg.UseCSVQuote {
		csvR := csv.NewReader(bomFreeReader)
		csvR.ReuseRecord = true
		csvR.LazyQuotes = true
		reader = csvR
	} else {
		reader = NewSimpleCSVReader(bomFreeReader)
	}

	writer := bufio.NewWriter(outFile)
	defer writer.Flush()

	dmp := dmpPool.Get().(*diffmatchpatch.DiffMatchPatch)
	defer dmpPool.Put(dmp)

	if err := executeProcessing(cfg, reader, writer, dmp, logger); err != nil {
		logger.Error("処理中にエラーが発生しました", "error", err)
		os.Exit(1)
	}

	if cfg.LineLimit > 0 {
		fmt.Printf("先頭 %d 行の差分ハイライト処理が完了しました: %s\n", cfg.LineLimit, cfg.OutputPath)
	} else {
		fmt.Printf("差分ハイライト処理が完了しました: %s\n", cfg.OutputPath)
	}
}

func executeProcessing(cfg Config, reader RecordReader, writer io.Writer, dmp *diffmatchpatch.DiffMatchPatch, logger *slog.Logger) error {
	if cfg.LightMode {
		csvWriter := csv.NewWriter(writer)
		if cfg.FormatHTML {
			logger.Info("HTML形式 (軽量リスト) で処理を開始します...")
			return processHTMLAsList(reader, writer, dmp, cfg.FontFamily, cfg.LineLimit, cfg.Headers, cfg.ExcelMode)
		}
		logger.Info("CSV形式 (軽量リスト) で処理を開始します...")
		err := processCSVAsList(reader, csvWriter, dmp, cfg.LineLimit, cfg.Headers)
		csvWriter.Flush()
		return err
	}

	csvWriter := csv.NewWriter(writer)
	if cfg.FormatHTML {
		logger.Info("HTML形式 (全データテーブル) で処理を開始します...")
		return processHTMLAsTable(reader, writer, dmp, cfg.FontFamily, cfg.LineLimit, cfg.Headers, cfg.EnableFilter, cfg.TrimSpaces, cfg.ExcelMode)
	}
	logger.Info("CSV形式 (全データ) で処理を開始します...")
	err := processCSVAsFull(reader, csvWriter, dmp, cfg.LineLimit, cfg.Headers, cfg.TrimSpaces)
	csvWriter.Flush()
	return err
}

func processCSVAsFull(reader RecordReader, writer *csv.Writer, dmp *diffmatchpatch.DiffMatchPatch, lineLimit int, headers []string, trimSpaces bool) error {
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
			// 変更点: isDiffがtrueでもtrimSpacesが有効ならトリムを行う
			if trimSpaces {
				trimDiffsRight(diffs)
			}

			if isDiff {
				outputRecord[i] = formatDiffsToText(diffs)
			} else {
				// isDiff=falseでもdiffsは返るので、formatDiffsToTextを使っても同じ結果になるが、
				// 念のため従来のロジック(単純なTrim)も残すか、統一するか。
				// ここではシンプルに、すでにトリム済みのdiffsを使う形に統一もできるが、
				// 既存ロジックへの影響を最小限にするため、分岐を残す。
				// ただし、parseDiffCellは非差分ならnilを返すので、非差分の場合は手動でトリム
				outputRecord[i] = strings.TrimRight(cell, "　")
			}
		}

		if err := writer.Write(outputRecord); err != nil {
			return fmt.Errorf("CSV行の書き込みに失敗 (line %d): %w", lineCount, err)
		}
	}
	return nil
}

func processCSVAsList(reader RecordReader, writer *csv.Writer, dmp *diffmatchpatch.DiffMatchPatch, lineLimit int, headers []string) error {
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

		for colNum, cell := range record {
			diffs, isDiff := parseDiffCell(cell, dmp)
			if isDiff {
				diffText := formatDiffsToText(diffs)
				colStr := fmt.Sprintf("%d", colNum+1)
				if headers != nil && colNum < len(headers) {
					colStr = fmt.Sprintf("%d:%s", colNum+1, headers[colNum])
				}
				row := []string{
					fmt.Sprintf("%d", lineCount),
					colStr,
					diffText,
				}
				if err := writer.Write(row); err != nil {
					return fmt.Errorf("軽量CSV行の書き込みに失敗 (line %d): %w", lineCount, err)
				}
			}
		}
	}
	return nil
}

func processHTMLAsList(reader RecordReader, writer io.Writer, dmp *diffmatchpatch.DiffMatchPatch, fontFamily string, lineLimit int, headers []string, exelMode bool) error {
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
				htmlDiff := formatDiffsToHTML(diffs, exelMode)
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

func processHTMLAsTable(reader RecordReader, writer io.Writer, dmp *diffmatchpatch.DiffMatchPatch, fontFamily string, lineLimit int, headers []string, enableFilter bool, trimSpaces bool, exelMode bool) error {
	writeHTMLHeaderTable(writer, fontFamily, headers, enableFilter)

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

		isRowAdd := true
		isRowDel := true
		hasDiff := false

		for i, cell := range record {
			diffs, isDiff := parseDiffCell(cell, dmp)

			// 変更点: isDiffがtrueでもtrimSpacesが有効ならトリムを行う
			if trimSpaces && isDiff {
				trimDiffsRight(diffs)
			}

			if isDiff {
				hasDiff = true
				outputCells[i] = formatDiffsToHTML(diffs, exelMode)

				if !isAllType(diffs, diffmatchpatch.DiffInsert) {
					isRowAdd = false
				}
				if !isAllType(diffs, diffmatchpatch.DiffDelete) {
					isRowDel = false
				}
			} else {
				if trimSpaces {
					outputCells[i] = html.EscapeString(strings.TrimRight(cell, "　"))
				} else {
					outputCells[i] = html.EscapeString(cell)
				}

				if cell != "" {
					isRowAdd = false
					isRowDel = false
				}
			}
		}

		rowClass := ""
		if hasDiff {
			if isRowAdd {
				rowClass = "diff-row-add"
			} else if isRowDel {
				rowClass = "diff-row-del"
			}
		}

		writeHTMLDataRowTable(writer, outputCells, rowClass)
	}
	io.WriteString(writer, "</tbody>\n")
	writeHTMLFooterTable(writer, enableFilter)
	return nil
}

// 変更点: Diffリストの末尾から全角スペースを削除する関数
func trimDiffsRight(diffs []diffmatchpatch.Diff) {
	if len(diffs) == 0 {
		return
	}
	lastIdx := len(diffs) - 1
	// 全角スペースのみをトリム
	diffs[lastIdx].Text = strings.TrimRight(diffs[lastIdx].Text, "　")
}

func isAllType(diffs []diffmatchpatch.Diff, t diffmatchpatch.Operation) bool {
	for _, d := range diffs {
		if d.Type != t {
			return false
		}
	}
	return true
}

func parseDiffCell(cell string, dmp *diffmatchpatch.DiffMatchPatch) ([]diffmatchpatch.Diff, bool) {
	if matches := diffRegexChange.FindStringSubmatch(cell); matches != nil {
		oldText := matches[1]
		if oldText == "" {
			oldText = matches[2]
		}
		newText := matches[3]
		diffs := dmp.DiffMain(oldText, newText, false)
		dmp.DiffCleanupSemantic(diffs)
		return diffs, true
	}
	if matches := diffRegexAdd.FindStringSubmatch(cell); matches != nil {
		return []diffmatchpatch.Diff{{Type: diffmatchpatch.DiffInsert, Text: matches[1]}}, true
	}
	if matches := diffRegexDel.FindStringSubmatch(cell); matches != nil {
		text := matches[1]
		if text == "" {
			text = matches[2]
		}
		return []diffmatchpatch.Diff{{Type: diffmatchpatch.DiffDelete, Text: text}}, true
	}

	return nil, false
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

func formatDiffsToHTML(diffs []diffmatchpatch.Diff, excelMode bool) string {
	var builder strings.Builder
	for _, diff := range diffs {
		escapedText := html.EscapeString(diff.Text)
		switch diff.Type {
		case diffmatchpatch.DiffEqual:
			builder.WriteString(escapedText)
		case diffmatchpatch.DiffDelete:
			if excelMode {
				// Excel向けのレガシー出力
				fmt.Fprintf(&builder, `<del class="diff-del" style="background-color: #ffebee;"><font color="#d32f2f"><s>%.*s</s></font></del>`, len(escapedText), escapedText)
			} else {
				// ブラウザ向けの標準出力（元に戻す）
				fmt.Fprintf(&builder, `<del class="diff-del">%.*s</del>`, len(escapedText), escapedText)
			}
		case diffmatchpatch.DiffInsert:
			if excelMode {
				// Excel向けのレガシー出力
				fmt.Fprintf(&builder, `<ins class="diff-add" style="background-color: #e8f5e9;"><font color="#388e3c"><b>%.*s</b></font></ins>`, len(escapedText), escapedText)
			} else {
				// ブラウザ向けの標準出力（元に戻す）
				fmt.Fprintf(&builder, `<ins class="diff-add">%.*s</ins>`, len(escapedText), escapedText)
			}
		}
	}
	return builder.String()
}

// --- HTMLヘルパー (リストモード) ---

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
	io.WriteString(w, `        .diff-del { color: #d32f2f; text-decoration: line-through; background-color: #ffebee; }
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

// --- HTMLヘルパー (テーブルモード) ---

func writeHTMLHeaderTable(w io.Writer, fontFamily string, headers []string, enableFilter bool) {
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
	io.WriteString(w, `        .diff-del { color: #d32f2f; text-decoration: line-through; background-color: #ffebee; }
        .diff-add { color: #388e3c; font-weight: bold; text-decoration: none; background-color: #e8f5e9; }
        
        .diff-row-add { background-color: #e6ffed !important; }
        .diff-row-del { background-color: #ffeef0 !important; }

        table { border-collapse: collapse; margin: 0; font-size: 0.9em; min-width: 100%; }
        
        th, td { 
            border: 1px solid #ccc; 
            padding: 8px 12px; 
            vertical-align: top; 
            text-align: left; 
            white-space: nowrap; 
        }
        th {
            position: sticky;
            top: 0;
            background-color: #f0f0f0;
            z-index: 10;
            box-shadow: 0 2px 2px -1px rgba(0, 0, 0, 0.4);
        }
        tbody tr:nth-child(odd) { background-color: #f9f9f9; }
        
        .table-wrapper {
            overflow: auto;
            max-height: 95vh;
            border: 1px solid #ccc;
        }
`)
	if enableFilter {
		io.WriteString(w, `
        .filter-input {
            width: 100%;
            box-sizing: border-box;
            padding: 4px;
            margin-top: 5px;
            border: 1px solid #ccc;
            border-radius: 3px;
            font-size: 0.9em;
            font-weight: normal;
        }
`)
	}

	io.WriteString(w, `    </style>
</head>
<body>
    <h1>差分比較結果 (全データ)</h1>
    <div class="table-wrapper">
        <table id="diffTable">
`)
	if headers != nil {
		io.WriteString(w, "<thead>\n<tr>\n")
		for _, h := range headers {
			fmt.Fprintf(w, "    <th>%s</th>\n", html.EscapeString(h))
		}
		io.WriteString(w, "</tr>\n</thead>\n")
	}
}

func writeHTMLDataRowTable(w io.Writer, cells []string, rowClass string) {
	if rowClass != "" {
		fmt.Fprintf(w, "<tr class=\"%s\">\n", rowClass)
	} else {
		io.WriteString(w, "<tr>\n")
	}
	for _, c := range cells {
		fmt.Fprintf(w, "    <td>%s</td>\n", c)
	}
	io.WriteString(w, "</tr>\n")
}

func writeHTMLFooterTable(w io.Writer, enableFilter bool) {
	io.WriteString(w, `        </table>
    </div>
`)

	if enableFilter {
		io.WriteString(w, `
<script>
(function() {
    const table = document.getElementById("diffTable");
    if (!table) return;

    const headers = table.querySelectorAll("thead th");
    if (headers.length === 0) return;

    headers.forEach((th, index) => {
        const input = document.createElement("input");
        input.type = "text";
        input.className = "filter-input";
        input.placeholder = "Filter...";
        
        input.addEventListener("click", function(e) { e.stopPropagation(); });
        input.addEventListener("input", function() {
            filterTable();
        });
        th.appendChild(document.createElement("br"));
        th.appendChild(input);
    });

    function filterTable() {
        const rows = table.querySelectorAll("tbody tr");
        const inputs = table.querySelectorAll(".filter-input");
        const filters = [];
        inputs.forEach((input, index) => {
            filters[index] = input.value.toLowerCase();
        });

        rows.forEach(row => {
            const cells = row.cells;
            let shouldShow = true;
            for (let i = 0; i < filters.length; i++) {
                const filterText = filters[i];
                if (!filterText) continue;
                if (cells[i]) {
                    const cellText = cells[i].innerText.toLowerCase();
                    if (!cellText.includes(filterText)) {
                        shouldShow = false;
                        break;
                    }
                }
            }
            row.style.display = shouldShow ? "" : "none";
        });
    }
})();
</script>
`)
	}

	io.WriteString(w, `</body>
</html>
`)
}
