package main

import (
	"bufio"
	"encoding/csv"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"os"
	"regexp"
	"strings"

	"github.com/sergi/go-diff/diffmatchpatch"
)

// diffRegex は [-old-]{+new+} の形式をキャプチャします。
var diffRegex = regexp.MustCompile(`^\s*\[\-(.*?)\-\]\{\+(.*?)\+\}\s*$`)

func main() {
	// 1. 起動時引数（フラグ）を定義
	inputPath := flag.String("i", "", "入力CSVファイルパス (必須)")
	outputPath := flag.String("o", "", "出力ファイルパス (必須)")
	formatHTML := flag.Bool("html", false, "HTML形式で出力する (デフォルトはCSV)")

	defaultFontStack := `"Helvetica Neue", Arial, "Hiragino Kaku Gothic ProN", "Hiragino Sans", Meiryo, sans-serif`
	fontFamily := flag.String("font", defaultFontStack, "HTML出力時に使用するCSSのfont-familyを指定します")

	// 変更点: -n (行数制限) フラグを追加
	lineLimit := flag.Int("n", 0, "処理する最大行数を指定します (0の場合は全行を処理)")

	flag.Parse()

	// 2. 必須引数のチェック
	if *inputPath == "" || *outputPath == "" {
		fmt.Fprintln(os.Stderr, "エラー: -i (入力パス) と -o (出力パス) は必須です。")
		flag.Usage() // ヘルプメッセージを表示
		os.Exit(1)
	}

	// 3. Diff-Match-Patch オブジェクトを生成
	dmp := diffmatchpatch.New()

	// 4. 入力ファイルを開く
	inFile, err := os.Open(*inputPath)
	if err != nil {
		log.Fatalf("入力ファイル '%s' を開けません: %v", *inputPath, err)
	}
	defer inFile.Close()

	// 5. 出力ファイルを作成
	outFile, err := os.Create(*outputPath)
	if err != nil {
		log.Fatalf("出力ファイル '%s' を作成できません: %v", *outputPath, err)
	}
	defer outFile.Close()

	// 6. バッファ付きWriterを使用 (CSV/HTML共通)
	writer := bufio.NewWriter(outFile)
	defer writer.Flush() // プログラム終了時にバッファを書き出す

	// 7. CSVリーダーを準備
	reader := csv.NewReader(inFile)
	reader.ReuseRecord = true // パフォーマンスのためレコードを再利用

	var processorErr error

	// 8. -html フラグに応じて処理を分岐
	if *formatHTML {
		// HTML処理
		log.Println("HTML形式で処理を開始します...")
		// 変更点: lineLimit を渡す
		processorErr = processHTML(reader, writer, dmp, *fontFamily, *lineLimit)
	} else {
		// CSV処理
		log.Println("CSV形式で処理を開始します...")
		csvWriter := csv.NewWriter(writer)
		// 変更点: lineLimit を渡す
		processorErr = processCSV(reader, csvWriter, dmp, *lineLimit)
		csvWriter.Flush()
	}

	if processorErr != nil {
		log.Fatalf("処理中にエラーが発生しました: %v", processorErr)
	}

	if *lineLimit > 0 {
		fmt.Printf("先頭 %d 行の差分ハイライト処理が完了しました: %s\n", *lineLimit, *outputPath)
	} else {
		fmt.Printf("差分ハイライト処理が完了しました: %s\n", *outputPath)
	}
}

// processCSV は、CSV to CSV の変換を行います。
// 変更点: lineLimit 引数を追加
func processCSV(reader *csv.Reader, writer *csv.Writer, dmp *diffmatchpatch.DiffMatchPatch, lineLimit int) error {
	var lineCount int // 変更点: 行数カウンター
	for {
		// 変更点: 行数制限のチェック
		if lineLimit > 0 && lineCount >= lineLimit {
			break // 制限に達したらループを抜ける
		}

		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("CSV読み取りエラー: %w", err)
		}
		lineCount++ // 変更点: 読み込み成功時にカウント

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
			return fmt.Errorf("CSV書き込みエラー: %w", err)
		}
	}
	return nil
}

// processHTML は、CSV to HTML の変換を行います。
// 変更点: lineLimit 引数を追加
func processHTML(reader *csv.Reader, writer *bufio.Writer, dmp *diffmatchpatch.DiffMatchPatch, fontFamily string, lineLimit int) error {
	// 1. HTMLヘッダーとCSSを書き込む
	writeHTMLHeader(writer, fontFamily)

	writer.WriteString("<tbody>\n")

	// 2. データ行を処理
	var lineCount int // 変更点: 行数カウンター
	for {
		// 変更点: 行数制限のチェック
		if lineLimit > 0 && lineCount >= lineLimit {
			break // 制限に達したらループを抜ける
		}

		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("CSVデータ読み取りエラー: %w", err)
		}
		lineCount++ // 変更点: 読み込み成功時にカウント

		// 差分処理
		outputCells := make([]string, len(record))
		for i, cell := range record {
			diffs, isDiff := parseDiffCell(cell, dmp)
			if isDiff {
				outputCells[i] = formatDiffsToHTML(diffs)
			} else {
				outputCells[i] = html.EscapeString(cell)
			}
		}
		// <tr><td>...</td></tr> を書き込む
		writeHTMLDataRow(writer, outputCells)
	}

	// 3. <tbody> とHTMLフッターを閉じる
	writer.WriteString("</tbody>\n")
	writeHTMLFooter(writer)
	return nil
}

// parseDiffCell はセルの内容を解析し、差分形式であればDiffリストを返します。
func parseDiffCell(cell string, dmp *diffmatchpatch.DiffMatchPatch) ([]diffmatchpatch.Diff, bool) {
	matches := diffRegex.FindStringSubmatch(cell)
	if matches == nil {
		return nil, false // 差分形式ではない
	}

	oldVal := matches[1]
	newVal := matches[2]

	diffs := dmp.DiffMain(oldVal, newVal, false)
	dmp.DiffCleanupSemantic(diffs) // 結果を読みやすく整形

	return diffs, true
}

// formatDiffsToText は差分を 共通[-del-]{+add+} のテキスト形式に変換します。
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

// formatDiffsToHTML は差分を <ins> と <del> タグを使ったHTMLに変換します。
func formatDiffsToHTML(diffs []diffmatchpatch.Diff) string {
	var builder strings.Builder
	for _, diff := range diffs {
		// HTMLインジェクションを防ぐため、常に html.EscapeString を通す
		escapedText := html.EscapeString(diff.Text)
		switch diff.Type {
		case diffmatchpatch.DiffEqual:
			builder.WriteString(escapedText)
		case diffmatchpatch.DiffDelete:
			// <del> タグで囲む
			fmt.Fprintf(&builder, `<del class="diff-del">%.*s</del>`, len(escapedText), escapedText)
		case diffmatchpatch.DiffInsert:
			// <ins> タグで囲む
			fmt.Fprintf(&builder, `<ins class="diff-add">%.*s</ins>`, len(escapedText), escapedText)
		}
	}
	return builder.String()
}

// --- HTMLヘルパー関数 ---

func writeHTMLHeader(w io.Writer, fontFamily string) {
	// セキュリティ対策: タグインジェクションを防ぐため、< と > を削除
	safeFontFamily := strings.ReplaceAll(fontFamily, "<", "")
	safeFontFamily = strings.ReplaceAll(safeFontFamily, ">", "")

	io.WriteString(w, `<!DOCTYPE html>
<html lang="ja">
<head>
    <meta charset="UTF-8">
    <title>差分比較結果</title>
    <style>
`)
	fmt.Fprintf(w, "        body { font-family: %s; }\n", safeFontFamily)
	io.WriteString(w, `
        table { border-collapse: collapse; margin: 20px 0; font-size: 0.9em; }
        td { border: 1px solid #ccc; padding: 8px 12px; vertical-align: top; }
        tbody tr:nth-child(odd) { background-color: #f9f9f9; }
        /* 差分ハイライト */
        .diff-del { 
            color: #d32f2f; /* 赤系 */
            text-decoration: line-through; 
            background-color: #ffebee;
        }
        .diff-add { 
            color: #388e3c; /* 緑系 */
            font-weight: bold; 
            text-decoration: none;
            background-color: #e8f5e9;
        }
    </style>
</head>
<body>
    <h1>差分比較結果</h1>
    <table>
`)
}

func writeHTMLDataRow(w io.Writer, cells []string) {
	io.WriteString(w, "<tr>\n")
	for _, c := range cells {
		// セルの中身 (c) は既にHTML形式かエスケープ済みなので、そのまま書き込む
		fmt.Fprintf(w, "    <td>%s</td>\n", c)
	}
	io.WriteString(w, "</tr>\n")
}

func writeHTMLFooter(w io.Writer) {
	io.WriteString(w, `</table>
</body>
</html>
`)
}
