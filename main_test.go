package main

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/sergi/go-diff/diffmatchpatch"
)

// --- テスト用のヘルパー ---

// (dmpPoolはmain.goで定義されているので、テスト側では不要)

// newCsvReader は文字列から *csv.Reader を作成します
func newCsvReader(s string) *csv.Reader {
	return csv.NewReader(strings.NewReader(s))
}

// runTest は、指定された設定と入力で executeProcessing を実行し、
// 出力バッファの内容を文字列として返します。
func runTest(t *testing.T, cfg Config, input string) (string, error) {
	t.Helper()

	reader := newCsvReader(input)
	var outBuf bytes.Buffer
	writer := bufio.NewWriter(&outBuf)

	dmp := dmpPool.Get().(*diffmatchpatch.DiffMatchPatch)
	defer dmpPool.Put(dmp)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	err := executeProcessing(cfg, reader, writer, dmp, logger)

	flushErr := writer.Flush()

	if err != nil {
		return outBuf.String(), err
	}
	if flushErr != nil {
		return outBuf.String(), flushErr
	}

	return outBuf.String(), nil
}

// --- コアロジックのテスト ---

func TestParseDiffCell(t *testing.T) {
	dmp := dmpPool.Get().(*diffmatchpatch.DiffMatchPatch)
	defer dmpPool.Put(dmp)

	t.Run("IsDiff", func(t *testing.T) {
		cell := "[-old text-]{+new text+}"
		diffs, isDiff := parseDiffCell(cell, dmp)
		if !isDiff {
			t.Fatal("isDiff should be true")
		}
		if len(diffs) != 3 {
			t.Fatalf("expected 3 diff segments, got %d", len(diffs))
		}
		if diffs[0].Type != diffmatchpatch.DiffDelete || diffs[0].Text != "old" {
			t.Errorf("Unexpected diffs[0].Type:%s diff[0].Text: %v", diffs[0].Type, diffs[0].Text)
		}
	})

	t.Run("NoDiff", func(t *testing.T) {
		cell := "just normal text"
		_, isDiff := parseDiffCell(cell, dmp)
		if isDiff {
			t.Fatal("isDiff should be false")
		}
	})
}

func TestFormatters(t *testing.T) {
	// (このテストは入力形式に依存しないため、変更なし)
	diffs := []diffmatchpatch.Diff{
		{Type: diffmatchpatch.DiffEqual, Text: "common"},
		{Type: diffmatchpatch.DiffDelete, Text: "del"},
		{Type: diffmatchpatch.DiffInsert, Text: "add"},
		{Type: diffmatchpatch.DiffEqual, Text: "<tag>"},
	}

	t.Run("FormatText", func(t *testing.T) {
		expected := "common[-del-]{+add+}<tag>"
		result := formatDiffsToText(diffs)
		if result != expected {
			t.Errorf("Expected %q, got %q", expected, result)
		}
	})

	t.Run("FormatHTML", func(t *testing.T) {
		expected := `common<del class="diff-del">del</del><ins class="diff-add">add</ins>&lt;tag&gt;`
		result := formatDiffsToHTML(diffs)
		if result != expected {
			t.Errorf("Expected %q, got %q", expected, result)
		}
	})
}

// --- 4つの主要処理パターンのテスト ---

const testInputDiff = `1,Apple,[-OK-]{+NG+},[-Note 1-]{+Note 2+}
2,Banana,OK,Note 3
3,Orange,[-NG-]{+OK+},Price 100`

const testInputNoDiff = `1,Apple,OK,Note 1
2,Banana,OK,Note 3`

var testHeaders = []string{"ID", "Item", "Status", "Memo"}

// (以降のテストは、testInputDiff を使うため、入力は自動的に修正されます)
// (期待値(expected)は変更ありません)

// 1. 全データ CSV (-light なし)
func TestProcessCSVAsFull(t *testing.T) {
	cfg := Config{LightMode: false, FormatHTML: false}

	t.Run("WithDiff_NoHeader", func(t *testing.T) {
		out, err := runTest(t, cfg, testInputDiff)
		if err != nil {
			t.Fatal(err)
		}
		expected := `1,Apple,[-OK-]{+NG+},Note [-1-]{+2+}
2,Banana,OK,Note 3
3,Orange,[-NG-]{+OK+},Price 100
`
		if out != expected {
			t.Errorf("Expected:\n%s\nGot:\n%s", expected, out)
		}
	})

	t.Run("WithDiff_WithHeader", func(t *testing.T) {
		cfgHeader := cfg
		cfgHeader.Headers = testHeaders
		out, err := runTest(t, cfgHeader, testInputDiff)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(out, "ID,Item,Status,Memo\n") {
			t.Error("Output should start with header row")
		}
		if !strings.Contains(out, "1,Apple,[-OK-]{+NG*}") {
			t.Error("Output missing diff data")
		}
	})

	t.Run("WithDiff_LineLimit", func(t *testing.T) {
		cfgLimit := cfg
		cfgLimit.LineLimit = 1
		out, err := runTest(t, cfgLimit, testInputDiff)
		if err != nil {
			t.Fatal(err)
		}
		expected := `1,Apple,[-OK-]{+NG+},Note [-1-]{+2+}
`
		if out != expected {
			t.Errorf("Expected:\n%s\nGot:\n%s", expected, out)
		}
	})
}

// 2. 全データ HTML (-light なし, -html あり)
func TestProcessHTMLAsTable(t *testing.T) {
	cfg := Config{LightMode: false, FormatHTML: true}

	t.Run("WithDiff_NoHeader", func(t *testing.T) {
		out, err := runTest(t, cfg, testInputDiff)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "<h1>差分比較結果 (全データ)</h1>") {
			t.Error("Missing title")
		}
		if strings.Contains(out, "<thead>") {
			t.Error("Should not contain <thead> when no headers provided")
		}
		if !strings.Contains(out, `<td>1</td>`) {
			t.Error("Missing data for row 1")
		}
		if !strings.Contains(out, `<td>NG<ins class="diff-add">OK</ins></td>`) {
			t.Error("Missing diff for row 3")
		}
	})

	t.Run("WithDiff_WithHeader", func(t *testing.T) {
		cfgHeader := cfg
		cfgHeader.Headers = testHeaders
		out, err := runTest(t, cfgHeader, testInputDiff)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "<thead>") {
			t.Error("Should contain <thead>")
		}
		if !strings.Contains(out, "<th>Status</th>") {
			t.Error("Missing header 'Status'")
		}
		if !strings.Contains(out, `<td>2</td>`) {
			t.Error("Missing data for row 2")
		}
	})
}

// 3. 軽量リスト CSV (-light あり)
func TestProcessCSVAsList(t *testing.T) {
	cfg := Config{LightMode: true, FormatHTML: false}

	t.Run("WithDiff_NoHeader", func(t *testing.T) {
		out, err := runTest(t, cfg, testInputDiff)
		if err != nil {
			t.Fatal(err)
		}
		expected := `Line,Column,DiffValue
1,3,[-OK-]{+NG+}
1,4,Note [-1-]{+2+}
3,3,[-NG-]{+OK+}
`
		if out != expected {
			t.Errorf("Expected:\n%s\nGot:\n%s", expected, out)
		}
	})

	t.Run("WithDiff_WithHeader", func(t *testing.T) {
		cfgHeader := cfg
		cfgHeader.Headers = testHeaders
		out, err := runTest(t, cfgHeader, testInputDiff)
		if err != nil {
			t.Fatal(err)
		}
		expected := `Line,Column,DiffValue
1,3:Status,[-OK-]{+NG+}
1,4:Memo,Note [-1-]{+2+}
3,3:Status,[-NG-]{+OK+}
`
		if out != expected {
			t.Errorf("Expected:\n%s\nGot:\n%s", expected, out)
		}
	})

	t.Run("NoDiff", func(t *testing.T) {
		out, err := runTest(t, cfg, testInputNoDiff)
		if err != nil {
			t.Fatal(err)
		}
		expected := "Line,Column,DiffValue\n" // ヘッダーのみ
		if out != expected {
			t.Errorf("Expected:\n%s\nGot:\n%s", expected, out)
		}
	})

	t.Run("HeaderMismatch", func(t *testing.T) {
		cfgHeader := cfg
		cfgHeader.Headers = []string{"ID", "Item"}
		// 変更点: 入力形式を修正
		input := `1,Apple,[-OK-]{+NG+}`
		out, err := runTest(t, cfgHeader, input)
		if err != nil {
			t.Fatal(err)
		}
		expected := `Line,Column,DiffValue
1,3,[-OK-]{+NG+}
`
		if out != expected {
			t.Errorf("Expected:\n%s\nGot:\n%s", expected, out)
		}
	})
}

// 4. 軽量リスト HTML (-light あり, -html あり)
func TestProcessHTMLAsList(t *testing.T) {
	cfg := Config{LightMode: true, FormatHTML: true}

	t.Run("WithDiff_NoHeader", func(t *testing.T) {
		out, err := runTest(t, cfg, testInputDiff)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "<h1>差分比較結果 (不一致のみ)</h1>") {
			t.Error("Missing title")
		}
		if !strings.Contains(out, `(Line 1, Col 3)`) {
			t.Error("Missing location for diff 1")
		}
		if !strings.Contains(out, `Note <del class="diff-del">1</del><ins class="diff-add">2</ins>`) {
			t.Error("Missing value for diff 2")
		}
		if !strings.Contains(out, `(Line 3, Col 3)`) {
			t.Error("Missing location for diff 3")
		}
		if strings.Contains(out, `(Line 2, Col`) {
			t.Error("Should not contain diff for line 2")
		}
	})

	t.Run("WithDiff_WithHeader", func(t *testing.T) {
		cfgHeader := cfg
		cfgHeader.Headers = testHeaders
		out, err := runTest(t, cfgHeader, testInputDiff)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, `(Line 1, Col 3:Status)`) {
			t.Error("Missing location with header for diff 1")
		}
		if !strings.Contains(out, `(Line 1, Col 4:Memo)`) {
			t.Error("Missing location with header for diff 2")
		}
	})

	t.Run("NoDiff", func(t *testing.T) {
		out, err := runTest(t, cfg, testInputNoDiff)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "<p class='no-diff'>差分は見つかりませんでした。</p>") {
			t.Error("Missing 'no diff' message")
		}
	})
}

// --- エラーハンドリングのテスト ---

// mockErrorReader は Read() でエラーを返します
type mockErrorReader struct{}

func (m *mockErrorReader) Read(p []byte) (n int, err error) {
	return 0, errors.New("mock read error")
}

// mockErrorWriter は Write() でエラーを返します
type mockErrorWriter struct{}

func (m *mockErrorWriter) Write(p []byte) (n int, err error) {
	return 0, errors.New("mock write error")
}

func TestIOErrors(t *testing.T) {
	dmp := dmpPool.Get().(*diffmatchpatch.DiffMatchPatch)
	defer dmpPool.Put(dmp)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// --- Read エラーのテスト ---
	t.Run("ReadError_CSVFull", func(t *testing.T) {
		reader := csv.NewReader(&mockErrorReader{})
		writer := csv.NewWriter(io.Discard)
		err := processCSVAsFull(reader, writer, dmp, 0, nil)
		if err == nil || !strings.Contains(err.Error(), "mock read error") {
			t.Errorf("Expected read error, got %v", err)
		}
	})
	t.Run("ReadError_CSVList", func(t *testing.T) {
		reader := csv.NewReader(&mockErrorReader{})
		writer := csv.NewWriter(io.Discard)
		err := processCSVAsList(reader, writer, dmp, 0, nil)
		if err == nil || !strings.Contains(err.Error(), "mock read error") {
			t.Errorf("Expected read error, got %v", err)
		}
	})
	t.Run("ReadError_HTMLTable", func(t *testing.T) {
		reader := csv.NewReader(&mockErrorReader{})
		writer := bufio.NewWriter(io.Discard)
		err := processHTMLAsTable(reader, writer, dmp, "", 0, nil)
		if err == nil || !strings.Contains(err.Error(), "mock read error") {
			t.Errorf("Expected read error, got %v", err)
		}
	})
	t.Run("ReadError_HTMLList", func(t *testing.T) {
		reader := csv.NewReader(&mockErrorReader{})
		writer := bufio.NewWriter(io.Discard)
		err := processHTMLAsList(reader, writer, dmp, "", 0, nil)
		if err == nil || !strings.Contains(err.Error(), "mock read error") {
			t.Errorf("Expected read error, got %v", err)
		}
	})

	// --- Write エラーのテスト ---
	t.Run("WriteError_CSVFull_Header", func(t *testing.T) {
		cfg := Config{LightMode: false, FormatHTML: false, Headers: testHeaders}
		reader := newCsvReader(testInputDiff)
		writer := &mockErrorWriter{} // バッファなしのモック
		err := executeProcessing(cfg, reader, writer, dmp, logger)
		if err == nil || !strings.Contains(err.Error(), "mock write error") {
			t.Errorf("Expected write error, got %v", err)
		}
	})
	t.Run("WriteError_CSVFull_Data", func(t *testing.T) {
		cfg := Config{LightMode: false, FormatHTML: false}
		reader := newCsvReader(testInputDiff)
		writer := &mockErrorWriter{}
		err := executeProcessing(cfg, reader, writer, dmp, logger)
		if err == nil || !strings.Contains(err.Error(), "mock write error") {
			t.Errorf("Expected write error, got %v", err)
		}
	})

	t.Run("WriteError_CSVList_Header", func(t *testing.T) {
		cfg := Config{LightMode: true, FormatHTML: false}
		reader := newCsvReader(testInputDiff)
		writer := &mockErrorWriter{}
		err := executeProcessing(cfg, reader, writer, dmp, logger)
		if err == nil || !strings.Contains(err.Error(), "mock write error") {
			t.Errorf("Expected write error, got %v", err)
		}
	})

	t.Run("WriteError_HTMLList_Header", func(t *testing.T) {
		cfg := Config{LightMode: true, FormatHTML: true}
		reader := newCsvReader(testInputDiff)
		writer := &mockErrorWriter{} // バッファなし
		err := executeProcessing(cfg, reader, writer, dmp, logger)
		if err == nil || !strings.Contains(err.Error(), "mock write error") {
			t.Errorf("Expected write error, got %v", err)
		}
	})
}
