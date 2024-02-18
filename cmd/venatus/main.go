package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
	"github.com/schollz/progressbar/v3"
	"github.com/sergi/go-diff/diffmatchpatch"
	"golang.org/x/sync/errgroup"
)

var (
	source = flag.String("source", "", "path to source repo")
	target = flag.String("target", "", "path to target repo")
	skip = flag.String("skip", "", "comma-separated files to skip")
	dmp = &diffmatchpatch.DiffMatchPatch{
		// Tuning: This variable is set so that we don't spend too long comparing very dissimilar files.
		// If files that are supposed to be alike are not getting scored highly, try increasing this.
		DiffTimeout:          4 * time.Second,
		DiffEditCost:         4,
		MatchThreshold:       0.5,
		MatchDistance:        1000,
		PatchDeleteThreshold: 0.5,
		PatchMargin:          4,
		MatchMaxBits:         32,		
	}
	// Don't bother comparing files whose basenames are more than this different.
	filenameSimilarityThreshold = 0.5
)

func main() {
	if err := mainErr(); err != nil {
		fmt.Fprintf(os.Stderr, "%v", err)
		os.Exit(1)
	}
}

func mainErr() error {
	flag.Parse()
	if *source == "" {
		return errors.New("--source not specified")
	}
	if *target == "" {
		return errors.New("--target not specified")
	}

	fmt.Println("Opening code files...")
	sourceFiles := openAllCodeFiles(*source)
	targetFiles := openAllCodeFiles(*target)
	skippedFiles := strings.Split(*skip, ",")
	for file := range targetFiles {
		for _, skippedFile := range skippedFiles {
			if strings.EqualFold(filepath.Base(file), skippedFile) {
				fmt.Printf("Skipping target file %q\n", file)
				delete(targetFiles, file)
				break
			}
		}
	}
	results := make(chan *findResult, len(targetFiles))

	fmt.Println("Comparing code files...")
	pb := progressbar.NewOptions(len(targetFiles),
	progressbar.OptionEnableColorCodes(true),
	progressbar.OptionFullWidth(),
	progressbar.OptionClearOnFinish())
	var errs errgroup.Group
	for path, fileContents := range targetFiles {
		path := path
		fileContents := fileContents
		errs.Go(func() error {
			result, err := findBestCandidate(path, fileContents, sourceFiles)
			if err != nil {
				return err
			}
			results <- result
			pb.Add(1)
			return nil
		})
	}
	err := errs.Wait()
	pb.Finish()
	if err != nil {
		return err
	}
	close(results)

	totalLineCount := 0

	// Read the results into a slice and sort them
	resultSlice := make([]*findResult, 0, len(targetFiles))
	for result := range results {
		resultSlice = append(resultSlice, result)
		totalLineCount += result.lineCount
	}
	sort.Slice(resultSlice, func (i, j int) bool {
		return resultSlice[i].lineCount > resultSlice[j].lineCount
		// return strings.Compare(resultSlice[i].filename, resultSlice[j].filename) < 0
	})

	overallScore := 0.0

	for _, result := range resultSlice {
		overallScore += result.matchSimilarity * (float64(result.lineCount) / float64(totalLineCount))
	}

	// Tabularize the results real nice
	tw := table.NewWriter()
	tw.SetStyle(table.StyleDouble)
	prefix := greatestCommonPrefix(*source, *target)
	tw.AppendHeader(table.Row{
		fmt.Sprintf("Path in %s", strings.TrimPrefix(*target, prefix)),
		fmt.Sprintf("Best match from %s", strings.TrimPrefix(*source, prefix)),
		"Score",
		"LoC",
	})
	for _, result := range resultSlice {
		tw.AppendRow(table.Row{
			strings.TrimPrefix(strings.TrimPrefix(result.filename, *target), "/"),
			strings.TrimPrefix(strings.TrimPrefix(result.matchedFilename, *source), "/"),
			percentage(result.matchSimilarity),
			result.lineCount,
		})
	}
	tw.AppendFooter(table.Row{
			"Total",
			"",
			percentage(overallScore),
			totalLineCount,
	})
	tw.SetRowPainter(func(row table.Row) text.Colors {
		pct := row[2].(percentage)
		if pct > 0.9 {
			return text.Colors{text.FgGreen}
		}
		if pct > 0.8 {
			return text.Colors{text.FgHiGreen}
		}
		if pct > 0.6 {
			return text.Colors{text.FgHiYellow}
		}
		return text.Colors{text.FgWhite}
	})
	fmt.Print(tw.Render())

	return nil
}

type percentage float64

func (p percentage) String() string {
	return fmt.Sprintf("%3.1f%%", p * 100.0)
}

func greatestCommonPrefix(a, b string) string {
	runesA := []rune(a)
	runesB := []rune(b)
	var sb strings.Builder
	for i, r := range runesA {
		if i >= len(runesB) || runesB[i] != r {
			break
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

type findResult struct {
	filename string
	matchedFilename string
	matchSimilarity float64
	lineCount int
}

func filenamesCloseEnough(name1, name2 string) bool {
	bname1 := filepath.Base(name1)
	bname2 := filepath.Base(name2)
	d := diff(bname1, bname2)
	return d.asPercentage() > filenameSimilarityThreshold
}

func findBestCandidate(path, fileContents string, source map[string]string) (*findResult, error) {
	bestResult := findResult{
		filename: path,
		matchedFilename: "N/A",
		matchSimilarity: 0,
		lineCount: strings.Count(fileContents, "\n"),
	}
	for sourcepath, contents := range source {
		if !filenamesCloseEnough(path, sourcepath) {
			continue
		}
		d := diff(fileContents, contents)
		thisSimilarity := d.asPercentage()
		if thisSimilarity > bestResult.matchSimilarity {
			bestResult.matchSimilarity = thisSimilarity
			bestResult.matchedFilename = sourcepath
		}
	}
	return &bestResult, nil
}

func openAllCodeFiles(path string) map[string]string {
	result := make(map[string]string)
	filepath.Walk(path, func(path string, info fs.FileInfo, err error) error {
		// Don't try to read into errors.
		if err != nil {
			return nil
		}
		// Don't try to read non-code files.
		if !(strings.HasSuffix(path, ".c") || strings.HasSuffix(path, ".h")) {
			return nil
		}
		code, err := readCodeFileNormalized(path)
		if err != nil {
			return err
		}
		result[path] = code
		return nil
	})
	return result
}


type result struct {
	levenshtein int
	length int
}

func (r result) asPercentage() float64 {
	return 1.0 - (float64(r.levenshtein)/float64(r.length))
}

func diff(contents1, contents2 string) *result {
	d := dmp.DiffMain(contents1, contents2, false)
	levenshtein := dmp.DiffLevenshtein(d)
	maxLen := len(contents1)
	if len(contents2) > maxLen {
		maxLen = len(contents2)
	}
	return &result{
		levenshtein: levenshtein,
		length: maxLen,
	}
}

func normalizeLine(line string) string {
	return strings.Join(strings.Fields(line), " ")
}

func readCodeFileNormalized(filename string) (string, error) {
	f, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var sb strings.Builder
	scanner := bufio.NewScanner(f)
	var comment, block bool
	for scanner.Scan() {
		comment, block = isComment(scanner.Text(), block)
		if !comment {
			sb.WriteString(normalizeLine(scanner.Text()))
			sb.WriteRune('\n')
		}
	}
	return sb.String(), nil
}

func isComment(line string, blockComment bool) (isComment, stillInBlockComment bool) {
	line = strings.Trim(line, " \t")
	if len(line) == 0 {
		return blockComment, blockComment
	}
	isComment = blockComment
	stillInBlockComment = blockComment
	if strings.HasPrefix(line, "/*") {
		isComment = true
		stillInBlockComment = true
	}
	if strings.Contains(line, "*/") {
		stillInBlockComment = false
	}
	if strings.HasPrefix(line, "//") {
		isComment = true
	}
	return
}