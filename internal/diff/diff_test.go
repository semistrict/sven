package diff

import (
	"reflect"
	"strings"
	"testing"
)

const sample = `diff --git a/main.go b/main.go
index 3b18e51..a2c4d1f 100644
--- a/main.go
+++ b/main.go
@@ -1,4 +1,5 @@ package main
 package main
 // entry point
 func main() {
+	println("debug")
 }
@@ -10,2 +11,2 @@ func other() {
-	a := 1
+	a := 2
diff --git a/logo.png b/logo.png
index 1111111..2222222 100644
Binary files a/logo.png and b/logo.png differ
diff --git a/old.txt b/new.txt
similarity index 100%
rename from old.txt
rename to new.txt
diff --git "a/tab\there.txt" "b/tab\there.txt"
new file mode 100644
index 0000000..e69de29
--- /dev/null
+++ "b/tab\there.txt"
@@ -0,0 +1 @@
+++ looks like a header but is content
diff --git a/only-removed.txt b/only-removed.txt
--- a/only-removed.txt
+++ b/only-removed.txt
@@ -1,2 +1 @@
 keep
-gone
\ No newline at end of file
diff --git a/deleted_test.go b/deleted_test.go
deleted file mode 100644
index 1234567..0000000
--- a/deleted_test.go
+++ /dev/null
@@ -1,2 +0,0 @@
-func TestX(t *testing.T) {
-}
`

func TestParse(t *testing.T) {
	files, err := Parse(strings.NewReader(sample))
	if err != nil {
		t.Fatal(err)
	}
	want := []File{
		{Path: "main.go", Hunks: []Hunk{
			{Header: "@@ -1,4 +1,5 @@ package main", Lines: []string{" package main", " // entry point", " func main() {", "+\tprintln(\"debug\")", " }"}},
			{Header: "@@ -10,2 +11,2 @@ func other() {", Lines: []string{"-\ta := 1", "+\ta := 2"}},
		}},
		{Path: "tab\there.txt", Hunks: []Hunk{
			{Header: "@@ -0,0 +1 @@", Lines: []string{"+++ looks like a header but is content"}},
		}},
		{Path: "only-removed.txt", Hunks: []Hunk{
			{Header: "@@ -1,2 +1 @@", Lines: []string{" keep", "-gone", `\ No newline at end of file`}},
		}},
		{Path: "deleted_test.go", Hunks: []Hunk{
			{Header: "@@ -1,2 +0,0 @@", Lines: []string{"-func TestX(t *testing.T) {", "-}"}},
		}},
	}
	if !reflect.DeepEqual(files, want) {
		t.Errorf("Parse =\n%#v\nwant\n%#v", files, want)
	}
}

func TestChunksKeepsSmallFileWhole(t *testing.T) {
	files, err := Parse(strings.NewReader(sample))
	if err != nil {
		t.Fatal(err)
	}
	want := []Chunk{{
		Text: "@@ -1,4 +1,5 @@ package main\n package main\n // entry point\n func main() {\n+\tprintln(\"debug\")\n }\n@@ -10,2 +11,2 @@ func other() {\n-\ta := 1\n+\ta := 2\n",
		Changes: []Change{
			{Line: 4, Text: "+\tprintln(\"debug\")"},
			{Line: 10, Text: "-\ta := 1"},
			{Line: 11, Text: "+\ta := 2"},
		},
	}}
	if got := files[0].Chunks(1000); !reflect.DeepEqual(got, want) {
		t.Errorf("Chunks = %#v, want %#v", got, want)
	}
}

func TestChunksSplitsBetweenHunks(t *testing.T) {
	f := File{Path: "f", Hunks: []Hunk{
		{Header: "@@ -1 +1,2 @@", Lines: []string{"+aaaa", "+bbbb"}},
		{Header: "@@ -9 +9 @@", Lines: []string{"+cccc"}},
	}}
	want := []Chunk{
		{Text: "@@ -1 +1,2 @@\n+aaaa\n+bbbb\n", Changes: []Change{{1, "+aaaa"}, {2, "+bbbb"}}},
		{Text: "@@ -9 +9 @@\n+cccc\n", Changes: []Change{{9, "+cccc"}}},
	}
	if got := f.Chunks(31); !reflect.DeepEqual(got, want) {
		t.Errorf("Chunks = %#v, want %#v", got, want)
	}
}

func TestChunksSplitsOversizedHunk(t *testing.T) {
	x := "+" + strings.Repeat("x", 30)
	f := File{Path: "f", Hunks: []Hunk{
		{Header: "@@ -5 +5,4 @@", Lines: []string{"+aaaa", "+bbbb", " keep", "+" + strings.Repeat("x", 30)}},
	}}
	want := []Chunk{
		{Text: "@@ -5 +5,4 @@\n+aaaa\n+bbbb\n", Changes: []Change{{5, "+aaaa"}, {6, "+bbbb"}}},
		{Text: "@@ -5 +5,4 @@\n keep\n", Changes: nil},
		{Text: "@@ -5 +5,4 @@\n" + x + "\n", Changes: []Change{{8, x}}},
	}
	if got := f.Chunks(26); !reflect.DeepEqual(got, want) {
		t.Errorf("Chunks = %#v, want %#v", got, want)
	}
}

func TestStat(t *testing.T) {
	f := File{Path: "a.go", Hunks: []Hunk{
		{Header: "@@ -1,3 +1,3 @@", Lines: []string{" keep", "-old", "+new", "+more"}},
		{Header: "@@ -9 +10 @@", Lines: []string{"-gone", `\ No newline at end of file`}},
	}}
	if added, removed := f.Stat(); added != 2 || removed != 2 {
		t.Errorf("Stat() = +%d -%d, want +2 -2", added, removed)
	}
}
