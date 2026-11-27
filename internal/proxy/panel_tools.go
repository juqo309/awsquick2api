package proxy

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
)

const panelToolsPrompt = `This dashboard can display artifacts from four local presentation tools. Use one whenever the user asks to create its output, including requests to draw, visualize, diagram, or chart something. These are dashboard tools, and the dashboard displays their output immediately.

Write any brief introductory text first, then end the reply with a single JSON object in this format: {"tool_calls":[{"name":"render_svg","input":{"title":"Example","svg":"<svg viewBox='0 0 100 100'><circle cx='50' cy='50' r='30' fill='#345'/></svg>"}}]}. Omit the object for ordinary text replies. You can put up to four tools in one object. Give the actual content in the arguments and avoid repeating it in prose. No follow-up turn is needed.

render_svg: {"title":"Short descriptive title","svg":"Complete static SVG under 64 KB"}. Use this for illustrations, diagrams and charts, even if the user just asks to show or draw something. Set a viewBox, give labels room, and use SVG presentation attributes like fill and stroke. Use local fragment references for arrows and gradients. Avoid scripts, style blocks, inline style attributes, images, and external URLs. When the user explicitly requests source code only, use an xml code block instead.
show_table: {"title":"Short descriptive title","columns":["Heading"],"rows":[["Cell value"]]}. Use for comparisons or structured data, up to 12 columns and 100 rows. Use one string, number, or boolean per cell.
make_checklist: {"title":"Short descriptive title","items":["One actionable task"]}. Use when checkable steps help the user track work, up to 30 items.
create_file: {"filename":"descriptive-name.md","content":"Complete contents"}. Use for downloadable .txt, .md, .csv, .tsv, or .json files under 64 KB. A download appears in chat. For a downloadable SVG, use render_svg.`

const panelArtifactLimit = 4
const panelArtifactBytes = 64 * 1024

var panelToolNames = map[string]bool{
	"render_svg": true, "show_table": true, "make_checklist": true, "create_file": true,
}

var panelCodeBlock = regexp.MustCompile("(?m)^ {0,3}```([\\w+.-]*)[ \\t]*\\r?\\n")
var panelFenceEnd = regexp.MustCompile("(?m)^ {0,3}```[ \\t]*\\r?$")
var panelJSONFence = regexp.MustCompile("(?i)```(?:json)?$")
var panelFilename = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9 ._-]{0,99}$`)

type panelArtifact struct {
	Kind     string     `json:"kind"`
	Title    string     `json:"title,omitempty"`
	Source   string     `json:"source,omitempty"`
	Columns  []string   `json:"columns,omitempty"`
	Rows     [][]string `json:"rows,omitempty"`
	Items    []string   `json:"items,omitempty"`
	Filename string     `json:"filename,omitempty"`
}

func panelReply(text string) (string, []panelArtifact) {
	var artifacts []panelArtifact
	if raw := extractToolJSON(text); raw != "" {
		before, after, _ := strings.Cut(text, raw)
		before = strings.TrimSpace(before)
		if fence := panelJSONFence.FindStringIndex(before); fence != nil {
			before = before[:fence[0]]
			after = strings.TrimPrefix(strings.TrimSpace(after), "```")
		}
		text = strings.TrimSpace(before + "\n\n" + after)
		calls := parseClientToolCalls(raw, panelToolNames, true)
		if len(calls) == 0 {
			artifacts = append(artifacts, panelArtifact{Kind: "error", Title: "The tool response could not be read. Ask Quick to try again."})
		}
		for _, call := range calls[:min(len(calls), panelArtifactLimit)] {
			if artifact, err := runPanelTool(call.Function); err == nil {
				artifacts = append(artifacts, artifact)
			} else {
				artifacts = append(artifacts, panelArtifact{Kind: "error", Title: err.Error()})
			}
		}
	} else {
		var gate toolStreamGate
		visible := gate.Push(text)
		if gate.Tool() {
			text = visible
			artifacts = append(artifacts, panelArtifact{Kind: "error", Title: "The tool response was incomplete. Ask Quick to try again."})
		}
	}
	if len(artifacts) >= panelArtifactLimit {
		return text, artifacts
	}
	if raw := strings.TrimSpace(text); strings.HasPrefix(raw, "<svg") {
		if svg, err := staticPanelSVG(raw); err == nil {
			return "", append(artifacts, panelArtifact{Kind: "svg", Title: "SVG preview", Source: svg})
		}
	}
	var visible strings.Builder
	for len(artifacts) < panelArtifactLimit {
		open := panelCodeBlock.FindStringSubmatchIndex(text)
		if open == nil {
			break
		}
		close := panelFenceEnd.FindStringIndex(text[open[1]:])
		if close == nil {
			break
		}
		start, end := open[1]+close[0], open[1]+close[1]
		visible.WriteString(text[:open[0]])
		if strings.EqualFold(text[open[2]:open[3]], "svg") {
			if svg, err := staticPanelSVG(text[open[1]:start]); err == nil {
				artifacts = append(artifacts, panelArtifact{Kind: "svg", Title: "SVG preview", Source: svg})
				text = text[end:]
				continue
			}
		}
		visible.WriteString(text[open[0]:end])
		text = text[end:]
	}
	visible.WriteString(text)
	return strings.TrimSpace(visible.String()), artifacts
}

func runPanelTool(call FunctionCall) (panelArtifact, error) {
	switch call.Name {
	case "render_svg":
		var args struct {
			Title string
			SVG   string
		}
		if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
			return panelArtifact{}, errors.New("The SVG input could not be read. Ask Quick to recreate it.")
		}
		svg, err := staticPanelSVG(args.SVG)
		if err != nil {
			return panelArtifact{}, errors.New("The SVG could not be previewed. Ask Quick to recreate it as a static SVG.")
		}
		return panelArtifact{Kind: "svg", Title: panelTitle(args.Title, "SVG preview"), Source: svg}, nil
	case "show_table":
		return panelTable(call.Arguments)
	case "make_checklist":
		var args struct {
			Title string
			Items []string
		}
		err := json.Unmarshal([]byte(call.Arguments), &args)
		if err != nil || len(args.Items) == 0 || len(args.Items) > 30 {
			return panelArtifact{}, errors.New("The checklist needs between 1 and 30 items.")
		}
		for i, item := range args.Items {
			args.Items[i] = strings.TrimSpace(item)
			if args.Items[i] == "" || len(item) > 2000 {
				return panelArtifact{}, errors.New("The checklist contains an empty or oversized item.")
			}
		}
		return panelArtifact{Kind: "checklist", Title: panelTitle(args.Title, "Checklist"), Items: args.Items}, nil
	case "create_file":
		var args struct {
			Filename string
			Content  *string
		}
		err := json.Unmarshal([]byte(call.Arguments), &args)
		args.Filename = strings.TrimSpace(args.Filename)
		switch strings.ToLower(path.Ext(args.Filename)) {
		case ".txt", ".md", ".csv", ".tsv", ".json":
		default:
			return panelArtifact{}, errors.New("The download needs a .txt, .md, .csv, .tsv, or .json filename.")
		}
		if err != nil || !panelFilename.MatchString(args.Filename) || args.Content == nil || len(*args.Content) > panelArtifactBytes {
			return panelArtifact{}, errors.New("The download has an invalid filename or is larger than 64 KB.")
		}
		return panelArtifact{Kind: "file", Filename: args.Filename, Source: *args.Content}, nil
	default:
		return panelArtifact{}, errors.New("The requested chat tool was not recognized.")
	}
}

func panelTable(arguments string) (panelArtifact, error) {
	var args struct {
		Title   string
		Columns []string
		Rows    [][]any
	}
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.UseNumber()
	if len(arguments) > panelArtifactBytes || decoder.Decode(&args) != nil || len(args.Columns) < 1 || len(args.Columns) > 12 || len(args.Rows) < 1 || len(args.Rows) > 100 {
		return panelArtifact{}, errors.New("The table needs 1 to 12 columns and 1 to 100 rows.")
	}
	for i, heading := range args.Columns {
		args.Columns[i] = strings.TrimSpace(heading)
		if args.Columns[i] == "" || len(heading) > 160 {
			return panelArtifact{}, errors.New("Each table column needs a short heading.")
		}
	}
	rows := make([][]string, len(args.Rows))
	for i, row := range args.Rows {
		if len(row) != len(args.Columns) {
			return panelArtifact{}, errors.New("Each table row needs one value per column.")
		}
		rows[i] = make([]string, len(row))
		for j, value := range row {
			switch value := value.(type) {
			case string, bool, json.Number:
				rows[i][j] = fmt.Sprint(value)
			case nil:
			default:
				return panelArtifact{}, errors.New("Table cells need text, numbers, or true/false values.")
			}
		}
	}
	return panelArtifact{Kind: "table", Title: panelTitle(args.Title, "Table"), Columns: args.Columns, Rows: rows}, nil
}

func panelTitle(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return strings.ToValidUTF8(value[:min(len(value), 200)], "")
}

const panelSVGNamespace = "http://www.w3.org/2000/svg"

var panelSVGElements = strings.Fields("svg g defs title desc path rect circle ellipse line polyline polygon text tspan linearGradient radialGradient stop clipPath mask marker pattern filter feDropShadow feGaussianBlur feOffset feBlend feColorMatrix feFlood feComposite feMerge feMergeNode")
var panelSVGAttributes = strings.Fields("id viewBox width height x y x1 y1 x2 y2 cx cy r rx ry fx fy fr dx dy d points transform preserveAspectRatio fill fill-rule fill-opacity stroke stroke-width stroke-linecap stroke-linejoin stroke-miterlimit stroke-dasharray stroke-dashoffset stroke-opacity opacity color paint-order vector-effect text-anchor dominant-baseline alignment-baseline baseline-shift textLength lengthAdjust font-size font-family font-weight font-style letter-spacing word-spacing clip-path clip-rule clipPathUnits mask maskUnits maskContentUnits marker marker-start marker-mid marker-end markerWidth markerHeight markerUnits orient refX refY offset stop-color stop-opacity gradientUnits gradientTransform spreadMethod patternUnits patternContentUnits patternTransform filter filterUnits primitiveUnits in in2 result mode stdDeviation flood-color flood-opacity type values operator k1 k2 k3 k4")

var panelLocalID = regexp.MustCompile(`^#[a-zA-Z0-9_.:-]+$`)
var panelLocalURL = regexp.MustCompile(`(?i)^url\(\s*(?:#[a-zA-Z0-9_.:-]+|"#[a-zA-Z0-9_.:-]+"|'#[a-zA-Z0-9_.:-]+')\s*\)$`)
var panelPaint = regexp.MustCompile(`^[#a-zA-Z0-9_.,%+() /-]+$`)

func staticPanelSVG(source string) (string, error) {
	source = strings.TrimSpace(source)
	if source == "" || len(source) > panelArtifactBytes {
		return "", errors.New("missing or oversized SVG")
	}
	decoder := xml.NewDecoder(strings.NewReader(source))
	var output bytes.Buffer
	encoder := xml.NewEncoder(&output)
	depth, nodes := 0, 0
	root := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		switch value := token.(type) {
		case xml.StartElement:
			name := value.Name.Local
			validNamespace := value.Name.Space == "" || value.Name.Space == panelSVGNamespace
			if depth == 0 {
				if root || name != "svg" || !validNamespace {
					return "", errors.New("expected one SVG document")
				}
				root = true
			}
			if !validNamespace || !svgContains(panelSVGElements, name) {
				if err := decoder.Skip(); err != nil {
					return "", err
				}
				continue
			}
			nodes++
			if depth > 64 || nodes > 3000 {
				return "", errors.New("SVG is too complex")
			}
			clean := xml.StartElement{Name: xml.Name{Local: name}}
			if depth == 0 {
				clean.Attr = append(clean.Attr, xml.Attr{Name: xml.Name{Local: "xmlns"}, Value: panelSVGNamespace})
			}
			seen := make(map[string]bool)
			for _, attr := range value.Attr {
				name := attr.Name.Local
				if seen[name] || !panelSVGAttribute(value.Name.Local, attr) {
					continue
				}
				seen[name] = true
				clean.Attr = append(clean.Attr, xml.Attr{Name: xml.Name{Local: name}, Value: attr.Value})
			}
			depth++
			token = clean
		case xml.EndElement:
			depth--
			token = xml.EndElement{Name: xml.Name{Local: value.Name.Local}}
		case xml.CharData:
			if depth == 0 {
				if strings.TrimSpace(string(value)) != "" {
					return "", errors.New("unexpected text outside SVG")
				}
				continue
			}
		case xml.Comment:
			continue
		case xml.ProcInst:
			if !root && value.Target == "xml" {
				continue
			}
			return "", errors.New("unsupported SVG processing instruction")
		default:
			return "", errors.New("unsupported SVG markup")
		}
		if err := encoder.EncodeToken(token); err != nil {
			return "", err
		}
	}
	if !root || depth != 0 || encoder.Close() != nil {
		return "", errors.New("incomplete SVG document")
	}
	return output.String(), nil
}

func svgContains(items []string, value string) bool {
	for _, item := range items {
		if value == item {
			return true
		}
	}
	return false
}

func panelSVGAttribute(element string, attr xml.Attr) bool {
	name, value := attr.Name.Local, strings.TrimSpace(attr.Value)
	if name == "href" {
		return (element == "linearGradient" || element == "radialGradient") &&
			(attr.Name.Space == "" || attr.Name.Space == "http://www.w3.org/1999/xlink") && panelLocalID.MatchString(value)
	}
	if attr.Name.Space != "" || !svgContains(panelSVGAttributes, name) {
		return false
	}
	switch name {
	case "fill", "stroke", "color", "stop-color", "flood-color", "clip-path", "mask", "marker", "marker-start", "marker-mid", "marker-end", "filter":
		if strings.Contains(strings.ToLower(value), "url") {
			return panelLocalURL.MatchString(value)
		}
		return panelPaint.MatchString(value)
	default:
		return true
	}
}
