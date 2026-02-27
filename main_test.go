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

// newReader は、Configに応じて適切なリーダーを返します
func newReader(s string, useQuote bool) RecordReader {
	r := strings.NewReader(s)
	if useQuote {
		csvR := csv.NewReader(r)
		csvR.LazyQuotes = true
		return csvR
	}
	return NewSimpleCSVReader(r)
}

// SimpleCSVReader のクォート除去ロジックと行全体処理のテスト
func TestSimpleCSVReader(t *testing.T) {
	t.Run("Quotes", func(t *testing.T) {
		input := `1,"Apple","","　　"`
		reader := NewSimpleCSVReader(strings.NewReader(input))
		record, err := reader.Read()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		expected := []string{"1", "Apple", "", "　　"}
		if len(record) != len(expected) {
			t.Fatalf("expected length %d, got %d", len(expected), len(record))
		}
		for i, val := range record {
			if val != expected[i] {
				t.Errorf("field[%d]: expected %q, got %q", i, expected[i], val)
			}
		}
	})

	t.Run("RowAdd", func(t *testing.T) {
		input := `{+"A","B","C"+}`
		reader := NewSimpleCSVReader(strings.NewReader(input))
		record, err := reader.Read()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// 各セルに {+ +} が分配されているか
		expected := []string{"{+A+}", "{+B+}", "{+C+}"}
		if len(record) != len(expected) {
			t.Fatalf("expected length %d, got %d", len(expected), len(record))
		}
		for i, val := range record {
			if val != expected[i] {
				t.Errorf("field[%d]: expected %q, got %q", i, expected[i], val)
			}
		}
	})
}

func runTest(t *testing.T, cfg Config, input string) (string, error) {
	t.Helper()

	reader := newReader(input, cfg.UseCSVQuote)
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

	t.Run("Change: [-A-]{+B+}", func(t *testing.T) {
		cell := "[-old text-]{+new text+}"
		diffs, isDiff := parseDiffCell(cell, dmp)
		if !isDiff {
			t.Fatal("isDiff should be true")
		}
		if len(diffs) != 3 {
			t.Fatalf("expected 3 diff segments, got %d", len(diffs))
		}
	})

	t.Run("Add: {+A+}", func(t *testing.T) {
		cell := "{+added text+}"
		diffs, isDiff := parseDiffCell(cell, dmp)
		if !isDiff {
			t.Fatal("isDiff should be true")
		}
		if len(diffs) != 1 || diffs[0].Type != diffmatchpatch.DiffInsert {
			t.Fatalf("expected 1 insert diff, got %v", diffs)
		}
		if diffs[0].Text != "added text" {
			t.Errorf("expected 'added text', got %q", diffs[0].Text)
		}
	})

	t.Run("Delete: [-A-]", func(t *testing.T) {
		cell := "[-deleted text-]"
		diffs, isDiff := parseDiffCell(cell, dmp)
		if !isDiff {
			t.Fatal("isDiff should be true")
		}
		if len(diffs) != 1 || diffs[0].Type != diffmatchpatch.DiffDelete {
			t.Fatalf("expected 1 delete diff, got %v", diffs)
		}
		if diffs[0].Text != "deleted text" {
			t.Errorf("expected 'deleted text', got %q", diffs[0].Text)
		}
	})

	t.Run("Delete: {-A-}", func(t *testing.T) {
		cell := "{-deleted text-}"
		diffs, isDiff := parseDiffCell(cell, dmp)
		if !isDiff {
			t.Fatal("isDiff should be true")
		}
		if len(diffs) != 1 || diffs[0].Type != diffmatchpatch.DiffDelete {
			t.Fatalf("expected 1 delete diff, got %v", diffs)
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
		result := formatDiffsToHTML(diffs, false)
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

// 末尾にスペースを含むテストデータ
const testInputWithSpaces = "1,Apple   ,OK,Note 1   \n2,Banana　　,OK,Note 3"

var testHeaders = []string{"ID", "Item", "Status", "Memo"}

// 1. 全データ CSV (-light なし)
func TestProcessCSVAsFull(t *testing.T) {
	cfg := Config{LightMode: false, FormatHTML: false}

	t.Run("WithDiff_NoHeader", func(t *testing.T) {
		out, err := runTest(t, cfg, testInputDiff)
		if err != nil {
			t.Fatal(err)
		}
		expected := `1,Apple,NG[-OK-],Note [-1-]{+2+}
2,Banana,OK,Note 3
3,Orange,OK[-NG-],Price 100
`
		if out != expected {
			t.Errorf("Expected:\n%s\nGot:\n%s", expected, out)
		}
	})

	t.Run("TrimSpaces", func(t *testing.T) {
		cfgTrim := cfg
		cfgTrim.TrimSpaces = true
		out, err := runTest(t, cfgTrim, testInputWithSpaces)
		if err != nil {
			t.Fatal(err)
		}
		// 末尾の全角スペースのみが削除されていることを確認 (Appleの半角スペースは残る)
		expected := `1,Apple   ,OK,Note 1   
2,Banana,OK,Note 3
`
		if out != expected {
			t.Errorf("Expected:\n%q\nGot:\n%q", expected, out)
		}
	})

	t.Run("RowAddTrim", func(t *testing.T) {
		cfgTrim := cfg
		cfgTrim.TrimSpaces = true
		// 行全体の追加で、中身に全角スペースがある場合
		input := `{+"A　　","B　"+}`
		out, err := runTest(t, cfgTrim, input)
		if err != nil {
			t.Fatal(err)
		}
		// {+A+},{+B+} のように、スペースが除去された状態で出力されることを期待
		expected := `{+A+},{+B+}
`
		if out != expected {
			t.Errorf("Expected:\n%q\nGot:\n%q", expected, out)
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
		if !strings.Contains(out, "1,Apple,NG[-OK-]") {
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
		expected := `1,Apple,NG[-OK-],Note [-1-]{+2+}
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

	t.Run("WithFilter", func(t *testing.T) {
		cfgFilter := cfg
		cfgFilter.EnableFilter = true
		cfgFilter.Headers = testHeaders
		out, err := runTest(t, cfgFilter, testInputDiff)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, `<script>`) {
			t.Error("Should contain script tag when filter is enabled")
		}
		if !strings.Contains(out, `.filter-input`) {
			t.Error("Should contain filter CSS")
		}
	})

	// 行全体追加・削除のスタイルテスト
	t.Run("RowStyle", func(t *testing.T) {
		// 行全体が {+ ... +} で囲まれた入力
		input := `{+"A","B"+}
[-"C","D"-]`
		out, err := runTest(t, cfg, input)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, `<tr class="diff-row-add">`) {
			t.Error("Missing diff-row-add class")
		}
		if !strings.Contains(out, `<tr class="diff-row-del">`) {
			t.Error("Missing diff-row-del class")
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
1,3,NG[-OK-]
1,4,Note [-1-]{+2+}
3,3,OK[-NG-]
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
1,3:Status,NG[-OK-]
1,4:Memo,Note [-1-]{+2+}
3,3:Status,OK[-NG-]
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
		input := `1,Apple,[-OK-]{+NG+}`
		out, err := runTest(t, cfgHeader, input)
		if err != nil {
			t.Fatal(err)
		}
		expected := `Line,Column,DiffValue
1,3,NG[-OK-]
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

func (m *mockErrorReader) Read() ([]string, error) {
	return nil, errors.New("mock read error")
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
		reader := &mockErrorReader{}
		writer := csv.NewWriter(io.Discard)
		err := processCSVAsFull(reader, writer, dmp, 0, nil, false)
		if err == nil || !strings.Contains(err.Error(), "mock read error") {
			t.Errorf("Expected read error, got %v", err)
		}
	})
	t.Run("ReadError_CSVList", func(t *testing.T) {
		reader := &mockErrorReader{}
		writer := csv.NewWriter(io.Discard)
		err := processCSVAsList(reader, writer, dmp, 0, nil)
		if err == nil || !strings.Contains(err.Error(), "mock read error") {
			t.Errorf("Expected read error, got %v", err)
		}
	})
	t.Run("ReadError_HTMLTable", func(t *testing.T) {
		reader := &mockErrorReader{}
		writer := bufio.NewWriter(io.Discard)
		err := processHTMLAsTable(reader, writer, dmp, "", 0, nil, false, false, false)
		if err == nil || !strings.Contains(err.Error(), "mock read error") {
			t.Errorf("Expected read error, got %v", err)
		}
	})
	t.Run("ReadError_HTMLList", func(t *testing.T) {
		reader := &mockErrorReader{}
		writer := bufio.NewWriter(io.Discard)
		err := processHTMLAsList(reader, writer, dmp, "", 0, nil, false)
		if err == nil || !strings.Contains(err.Error(), "mock read error") {
			t.Errorf("Expected read error, got %v", err)
		}
	})

	// --- Write エラーのテスト ---
	t.Run("WriteError_CSVFull_Header", func(t *testing.T) {
		cfg := Config{LightMode: false, FormatHTML: false, Headers: testHeaders}
		reader := newReader(testInputDiff, false)
		writer := &mockErrorWriter{}
		err := executeProcessing(cfg, reader, writer, dmp, logger)
		if err == nil || !strings.Contains(err.Error(), "mock write error") {
			t.Errorf("Expected write error, got %v", err)
		}
	})
	t.Run("WriteError_CSVFull_Data", func(t *testing.T) {
		cfg := Config{LightMode: false, FormatHTML: false}
		reader := newReader(testInputDiff, false)
		writer := &mockErrorWriter{}
		err := executeProcessing(cfg, reader, writer, dmp, logger)
		if err == nil || !strings.Contains(err.Error(), "mock write error") {
			t.Errorf("Expected write error, got %v", err)
		}
	})

	t.Run("WriteError_CSVList_Header", func(t *testing.T) {
		cfg := Config{LightMode: true, FormatHTML: false}
		reader := newReader(testInputDiff, false)
		writer := &mockErrorWriter{}
		err := executeProcessing(cfg, reader, writer, dmp, logger)
		if err == nil || !strings.Contains(err.Error(), "mock write error") {
			t.Errorf("Expected write error, got %v", err)
		}
	})

	t.Run("WriteError_HTMLList_Header", func(t *testing.T) {
		cfg := Config{LightMode: true, FormatHTML: true}
		reader := newReader(testInputDiff, false)
		writer := &mockErrorWriter{}
		err := executeProcessing(cfg, reader, writer, dmp, logger)
		if err == nil || !strings.Contains(err.Error(), "mock write error") {
			t.Errorf("Expected write error, got %v", err)
		}
	})
}
