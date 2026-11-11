package request

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// BodyFormat est le format de sérialisation d'un corps de requête.
type BodyFormat string

const (
	FormatJSON      BodyFormat = "json"
	FormatYAML      BodyFormat = "yaml"
	FormatXML       BodyFormat = "xml"
	FormatMultipart BodyFormat = "multipart"
	FormatForm      BodyFormat = "form"
	FormatText      BodyFormat = "text"
)

// validFormats liste tous les formats pris en charge.
var validFormats = []BodyFormat{FormatJSON, FormatYAML, FormatXML, FormatMultipart, FormatForm, FormatText}

// ParseBodyFormat valide et normalise un nom de format.
func ParseBodyFormat(raw string) (BodyFormat, error) {
	f := BodyFormat(strings.ToLower(strings.TrimSpace(raw)))
	switch f {
	case FormatJSON, FormatYAML, FormatXML, FormatMultipart, FormatForm, FormatText:
		return f, nil
	}
	return "", fmt.Errorf("format non pris en charge %q (formats : json, yaml, xml, multipart, form, text)", raw)
}

// currentFormat détecte le format actuel du constructeur à partir du
// Content-Type et, quand il est absent ou incohérent, en inspectant le corps.
func (b *Builder) currentFormat() BodyFormat {
	if b.HasForm() {
		return FormatMultipart
	}
	ct := strings.ToLower(b.ContentType())
	body := b.Body()
	switch {
	case strings.HasPrefix(ct, "application/json") && json.Valid(body):
		return FormatJSON
	case strings.HasPrefix(ct, "application/yaml"), strings.HasPrefix(ct, "text/yaml"):
		return FormatYAML
	case strings.HasPrefix(ct, "application/xml"), strings.HasPrefix(ct, "text/xml"):
		return FormatXML
	case IsMultipartContentType(ct):
		return FormatMultipart
	case strings.HasPrefix(ct, "application/x-www-form-urlencoded"):
		return FormatForm
	case strings.HasPrefix(ct, "text/plain"):
		return FormatText
	}
	// Content-Type absent, générique, ou incohérent avec le corps : inspecter.
	if json.Valid(body) {
		return FormatJSON
	}
	if data, err := yamlUnmarshal(string(body)); err == nil {
		switch data.(type) {
		case map[string]any, []any:
			return FormatYAML
		}
	}
	return FormatText
}

// bodyData est la représentation intermédiaire d'un corps avant conversion.
type bodyData struct {
	kind   string // "structured", "multipart", "form", "text"
	data   any    // structured: map[string]any / []any / scalar
	parts  []FormPart
	values url.Values
	raw    []byte
}

// parseBody lit le corps courant du constructeur et le représente de manière
// normalisée. Le corps original n'est jamais modifié.
func (b *Builder) parseBody() (bodyData, error) {
	bd := bodyData{}
	switch b.currentFormat() {
	case FormatJSON:
		var data any
		body := b.Body()
		if len(body) == 0 {
			bd.kind = "structured"
			bd.data = map[string]any{}
			return bd, nil
		}
		if err := json.Unmarshal(body, &data); err != nil {
			// Pas du JSON utilisable : fallback texte.
			bd.kind = "text"
			bd.raw = append([]byte(nil), body...)
			return bd, nil
		}
		bd.kind = "structured"
		bd.data = data
		return bd, nil
	case FormatYAML:
		body := b.Body()
		if len(body) == 0 {
			bd.kind = "structured"
			bd.data = map[string]any{}
			return bd, nil
		}
		data, err := yamlUnmarshal(string(body))
		if err != nil {
			bd.kind = "text"
			bd.raw = append([]byte(nil), body...)
			return bd, nil
		}
		bd.kind = "structured"
		bd.data = data
		return bd, nil
	case FormatXML:
		body := b.Body()
		if len(body) == 0 {
			bd.kind = "structured"
			bd.data = map[string]any{}
			return bd, nil
		}
		data, err := xmlUnmarshal(body)
		if err != nil {
			bd.kind = "text"
			bd.raw = append([]byte(nil), body...)
			return bd, nil
		}
		bd.kind = "structured"
		bd.data = data
		return bd, nil
	case FormatMultipart:
		// Privilégier la structure formulaire en mémoire quand elle existe.
		if b.HasForm() {
			bd.kind = "multipart"
			bd.parts = b.FormParts()
			return bd, nil
		}
		ct := b.ContentType()
		parts, err := ParseMultipart(b.Body(), ct)
		if err != nil {
			return bodyData{}, fmt.Errorf("impossible de parser le multipart courant : %w", err)
		}
		bd.kind = "multipart"
		bd.parts = parts
		return bd, nil
	case FormatForm:
		body := b.Body()
		values, err := url.ParseQuery(string(body))
		if err != nil {
			return bodyData{}, fmt.Errorf("impossible de parser le formulaire encodé courant : %w", err)
		}
		bd.kind = "form"
		bd.values = values
		return bd, nil
	default: // text
		bd.kind = "text"
		bd.raw = append([]byte(nil), b.Body()...)
		return bd, nil
	}
}

// ConvertBody convertit le corps du constructeur vers le format cible, met à
// jour le Content-Type et préserve les autres en-têtes. En cas d'erreur, le
// constructeur est laissé tel quel.
func (b *Builder) ConvertBody(format BodyFormat) error {
	origin := b.currentFormat()
	if origin == format {
		// Même format : rien à faire, le Content-Type est déjà correct.
		return nil
	}

	bd, err := b.parseBody()
	if err != nil {
		return err
	}

	body, contentType, parts, err := serializeBody(bd, format)
	if err != nil {
		return err
	}

	// Appliquer la conversion uniquement si la sérialisation a réussi.
	b.body = body
	b.bodySet = len(body) > 0 || b.bodySet
	b.bodyMethod = b.method
	if parts != nil {
		b.form = &multipartForm{parts: parts, boundary: ""}
	} else {
		// Ne pas conserver un formulaire obsolète quand on quitte le multipart.
		if origin == FormatMultipart {
			b.form = nil
		}
	}
	_ = b.SetHeader("Content-Type", contentType)
	return nil
}

// serializeBody produit le corps, le Content-Type et (pour multipart) les
// parties à partir de la représentation normalisée.
func serializeBody(bd bodyData, target BodyFormat) ([]byte, string, []FormPart, error) {
	switch target {
	case FormatJSON:
		return serializeJSON(bd)
	case FormatYAML:
		return serializeYAML(bd)
	case FormatXML:
		return serializeXML(bd)
	case FormatMultipart:
		return serializeMultipart(bd)
	case FormatForm:
		return serializeForm(bd)
	case FormatText:
		return serializeText(bd)
	}
	return nil, "", nil, fmt.Errorf("format cible inconnu %s", target)
}

// serializeJSON sérialise n'importe quelle représentation en JSON.
func serializeJSON(bd bodyData) ([]byte, string, []FormPart, error) {
	var data any
	switch bd.kind {
	case "structured":
		data = normalizeJSON(bd.data)
	case "multipart":
		data = multipartToJSON(bd.parts)
	case "form":
		data = formToJSON(bd.values)
	case "text":
		data = string(bd.raw)
	}
	out, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return nil, "", nil, err
	}
	out = append(out, '\n')
	return out, "application/json", nil, nil
}

// serializeYAML sérialise en YAML (sous-ensemble JSON-compatible).
func serializeYAML(bd bodyData) ([]byte, string, []FormPart, error) {
	var data any
	switch bd.kind {
	case "structured":
		data = normalizeJSON(bd.data)
	case "multipart":
		data = multipartToJSON(bd.parts)
	case "form":
		data = formToJSON(bd.values)
	case "text":
		data = string(bd.raw)
	}
	out := yamlMarshal(data, 0)
	return []byte(out), "application/yaml", nil, nil
}

// serializeXML sérialise en XML.
func serializeXML(bd bodyData) ([]byte, string, []FormPart, error) {
	var data any
	switch bd.kind {
	case "structured":
		data = normalizeJSON(bd.data)
	case "multipart":
		data = multipartToJSON(bd.parts)
	case "form":
		data = formToJSON(bd.values)
	case "text":
		data = string(bd.raw)
	}
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	if err := xmlWriteValue(&buf, "root", data, 0); err != nil {
		return nil, "", nil, err
	}
	return buf.Bytes(), "application/xml", nil, nil
}

// serializeMultipart construit un multipart/form-data. Les champs texte
// deviennent des parties de formulaire ; les fichiers existants sont
// préservés. Les données binaires non représentables provoquent une erreur.
func serializeMultipart(bd bodyData) ([]byte, string, []FormPart, error) {
	var parts []FormPart
	switch bd.kind {
	case "multipart":
		parts = append([]FormPart{}, bd.parts...)
	case "structured":
		m, ok := bd.data.(map[string]any)
		if !ok {
			return nil, "", nil, fmt.Errorf("conversion vers multipart : le corps doit être un objet, obtenu %T", bd.data)
		}
		for _, k := range sortedKeys(m) {
			v := m[k]
			parts = append(parts, FormPart{Name: k, Value: renderScalar(v)})
		}
	case "form":
		for _, k := range sortedKeysString(bd.values) {
			for _, v := range bd.values[k] {
				parts = append(parts, FormPart{Name: k, Value: v})
			}
		}
	case "text":
		parts = append(parts, FormPart{Name: "body", Value: string(bd.raw)})
	}
	f := &multipartForm{parts: parts}
	body, contentType := (&Builder{form: f}).BuildMultipart()
	return body, contentType, parts, nil
}

// serializeForm encode en application/x-www-form-urlencoded. Les fichiers
// d'un multipart source provoquent une erreur (pas de représentation
// réversible sans perte).
func serializeForm(bd bodyData) ([]byte, string, []FormPart, error) {
	values := url.Values{}
	switch bd.kind {
	case "form":
		values = bd.values
	case "structured":
		m, ok := bd.data.(map[string]any)
		if !ok {
			return nil, "", nil, fmt.Errorf("conversion vers form-urlencoded : le corps doit être un objet, obtenu %T", bd.data)
		}
		for _, k := range sortedKeys(m) {
			values.Add(k, renderScalar(m[k]))
		}
	case "multipart":
		for _, p := range bd.parts {
			if p.IsFile {
				return nil, "", nil, fmt.Errorf("conversion vers form-urlencoded : le fichier %q ne peut être représenté sans perte", p.FileName)
			}
			values.Add(p.Name, p.Value)
		}
	case "text":
		values.Set("body", string(bd.raw))
	}
	return []byte(values.Encode()), "application/x-www-form-urlencoded", nil, nil
}

// serializeText renvoie le corps brut tel quel (ou une représentation texte
// des données structurées).
func serializeText(bd bodyData) ([]byte, string, []FormPart, error) {
	switch bd.kind {
	case "text":
		return append([]byte(nil), bd.raw...), "text/plain", nil, nil
	case "structured":
		out, err := json.MarshalIndent(bd.data, "", "  ")
		if err != nil {
			return nil, "", nil, err
		}
		return append(out, '\n'), "text/plain", nil, nil
	case "form":
		return []byte(bd.values.Encode()), "text/plain", nil, nil
	case "multipart":
		var lines []string
		for _, p := range bd.parts {
			if p.IsFile {
				lines = append(lines, fmt.Sprintf("%s: fichier %s (%s, %d octets)", p.Name, p.FileName, p.ContentType, len(p.Data)))
			} else {
				lines = append(lines, fmt.Sprintf("%s: %s", p.Name, p.Value))
			}
		}
		return []byte(strings.Join(lines, "\n") + "\n"), "text/plain", nil, nil
	}
	return nil, "", nil, fmt.Errorf("représentation texte indisponible")
}

// normalizeJSON nettoie une valeur parsée pour une sérialisation JSON stable.
func normalizeJSON(v any) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, v := range val {
			out[k] = normalizeJSON(v)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, v := range val {
			out[i] = normalizeJSON(v)
		}
		return out
	default:
		return v
	}
}

// renderScalar convertit une valeur en représentation texte pour un champ.
func renderScalar(v any) string {
	switch val := v.(type) {
	case string:
		return val
	case []byte:
		return string(val)
	case float64:
		return strconv.FormatFloat(val, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(val)
	case nil:
		return ""
	default:
		out, _ := json.Marshal(val)
		return string(out)
	}
}

// multipartToJSON convertit des parties multipart en objet JSON. Les champs
// répétés deviennent des tableaux ; les fichiers sont encodés en base64 avec
// leurs métadonnées pour rester réversibles.
func multipartToJSON(parts []FormPart) any {
	obj := make(map[string]any)
	for _, p := range parts {
		if p.IsFile {
			obj[p.Name] = map[string]any{
				"filename":     p.FileName,
				"content_type": p.ContentType,
				"data":         base64Encode(p.Data),
			}
			continue
		}
		existing, ok := obj[p.Name]
		if !ok {
			obj[p.Name] = p.Value
			continue
		}
		switch arr := existing.(type) {
		case []any:
			obj[p.Name] = append(arr, p.Value)
		default:
			obj[p.Name] = []any{existing, p.Value}
		}
	}
	return obj
}

// formToJSON convertit des valeurs d'un formulaire encodé en objet JSON.
func formToJSON(values url.Values) any {
	obj := make(map[string]any, len(values))
	for k, vs := range values {
		if len(vs) == 1 {
			obj[k] = vs[0]
		} else {
			obj[k] = vs
		}
	}
	return obj
}

// base64Encode encode des octets en base64 standard (sans padding de ligne).
func base64Encode(data []byte) string {
	return encodeBase64(data)
}

// sortedKeys retourne les clés d'une map triées.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sortedKeysString retourne les clés d'un url.Values triées.
func sortedKeysString(v url.Values) []string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---------------------------------------------------------------------------
// YAML minimal (émetteur et parseur pour le sous-ensemble JSON-compatible)
// ---------------------------------------------------------------------------

func yamlMarshal(v any, indent int) string {
	var buf strings.Builder
	yamlMarshalValue(&buf, v, indent)
	return buf.String()
}

func yamlMarshalValue(buf *strings.Builder, v any, indent int) {
	sp := strings.Repeat(" ", indent)
	switch val := v.(type) {
	case map[string]any:
		if len(val) == 0 {
			buf.WriteString("{}\n")
			return
		}
		for _, k := range sortedKeys(val) {
			buf.WriteString(sp + k + ": ")
			yamlMarshalValue(buf, val[k], indent+2)
		}
	case []any:
		if len(val) == 0 {
			buf.WriteString("[]\n")
			return
		}
		for _, item := range val {
			buf.WriteString(sp + "- ")
			switch item.(type) {
			case map[string]any, []any:
				buf.WriteString("\n")
				yamlMarshalValue(buf, item, indent+2)
			default:
				buf.WriteString(renderScalar(item) + "\n")
			}
		}
	case nil:
		buf.WriteString("null\n")
	case string:
		buf.WriteString(yamlQuote(val) + "\n")
	default:
		buf.WriteString(renderScalar(val) + "\n")
	}
}

func yamlQuote(s string) string {
	if s == "" {
		return `""`
	}
	needQuote := false
	for _, r := range s {
		if r == ':' || r == '#' || r == '[' || r == ']' || r == '{' || r == '}' || r == ',' || r == '&' || r == '*' || r == '?' || r == '|' || r == '<' || r == '>' || r == '!' || r == '%' || r == '@' || r == '`' {
			needQuote = true
			break
		}
	}
	if strings.ContainsAny(s, " \t\n\r") {
		needQuote = true
	}
	if needQuote {
		var b strings.Builder
		b.WriteRune('"')
		for _, r := range s {
			switch r {
			case '"':
				b.WriteString(`\"`)
			case '\\':
				b.WriteString(`\\`)
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case '\t':
				b.WriteString(`\t`)
			default:
				b.WriteRune(r)
			}
		}
		b.WriteRune('"')
		return b.String()
	}
	return s
}

// yamlUnmarshal parse un YAML minimal (maps, listes, scalars).
func yamlUnmarshal(s string) (any, error) {
	lines := strings.Split(s, "\n")
	data, _, err := yamlParseBlock(lines, 0, 0)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func yamlParseBlock(lines []string, start, indent int) (any, int, error) {
	// Première ligne non vide : elle détermine si le bloc est une liste ou
	// une carte. Une liste imbriquée peut suivre une clé de carte.
	first := -1
	for i := start; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "" {
			first = i
			break
		}
	}
	if first < 0 {
		return nil, start, nil
	}
	expected := strings.Repeat(" ", indent)
	isList := strings.HasPrefix(lines[first], expected+"- ")

	if isList {
		arr := []any{}
		i := start
		for i < len(lines) {
			line := lines[i]
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				i++
				continue
			}
			expected := strings.Repeat(" ", indent)
			if !strings.HasPrefix(line, expected) || !strings.HasPrefix(trimmed, "- ") {
				break
			}
			content := strings.TrimPrefix(trimmed, "- ")
			i++
			if content == "" {
				// Liste imbriquée ou map sur les lignes suivantes.
				if i < len(lines) {
					next := lines[i]
					nextIndent := strings.Count(next, " ")
					if nextIndent > indent {
						val, ni, err := yamlParseBlock(lines, i, nextIndent)
						if err != nil {
							return nil, 0, err
						}
						arr = append(arr, val)
						i = ni
						continue
					}
				}
				arr = append(arr, nil)
				continue
			}
			if strings.Contains(content, ": ") {
				// Inline map : "- key: value"
				idx := strings.Index(content, ": ")
				key := content[:idx]
				valStr := content[idx+2:]
				childIndent := strings.Count(lines[i-1], " ") + 2
				if i < len(lines) && strings.Count(lines[i], " ") >= childIndent {
					val, ni, err := yamlParseBlock(lines, i, childIndent)
					if err != nil {
						return nil, 0, err
					}
					arr = append(arr, map[string]any{key: val})
					i = ni
				} else {
					arr = append(arr, map[string]any{key: parseScalar(valStr)})
				}
				continue
			}
			arr = append(arr, parseScalar(content))
		}
		return arr, i, nil
	}

	// Carte.
	obj := map[string]any{}
	i := start
	for i < len(lines) {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			i++
			continue
		}
		expected := strings.Repeat(" ", indent)
		if indent > 0 && !strings.HasPrefix(line, expected) {
			break
		}
		if indent == 0 && !strings.HasPrefix(line, "- ") && !strings.Contains(line, ":") {
			break
		}
		if strings.HasPrefix(trimmed, "- ") {
			break
		}
		idx := strings.Index(trimmed, ": ")
		if idx < 0 {
			idx = strings.Index(trimmed, ":")
			if idx < 0 {
				i++
				continue
			}
		}
		key := trimmed[:idx]
		valStr := trimmed[idx+1:]
		if strings.HasPrefix(valStr, " ") {
			valStr = valStr[1:]
		}
		i++
		childIndent := strings.Count(line, " ") + 2
		if valStr == "" && i < len(lines) {
			nextIndent := strings.Count(lines[i], " ")
			if nextIndent > childIndent-2 {
				val, ni, err := yamlParseBlock(lines, i, childIndent)
				if err != nil {
					return nil, 0, err
				}
				obj[key] = val
				i = ni
				continue
			}
		}
		obj[key] = parseScalar(valStr)
	}
	return obj, i, nil
}

func parseScalar(s string) any {
	s = strings.TrimSpace(s)
	if s == "" || s == "null" || s == "~" {
		return nil
	}
	if s == "true" {
		return true
	}
	if s == "false" {
		return false
	}
	// Entier.
	if _, err := strconv.Atoi(s); err == nil {
		if i, err := strconv.Atoi(s); err == nil {
			return i
		}
	}
	// Flottant.
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	// Retirer les guillemets.
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		unquoted := strings.ReplaceAll(s[1:len(s)-1], `\"`, `"`)
		unquoted = strings.ReplaceAll(unquoted, `\\`, `\`)
		return unquoted
	}
	return s
}

// ---------------------------------------------------------------------------
// XML (sérialisation de map[string]any / []any / scalars)
// ---------------------------------------------------------------------------

func xmlWriteValue(buf *bytes.Buffer, name string, v any, indent int) error {
	sp := strings.Repeat("  ", indent)
	eName := xmlEscape(name)
	switch val := v.(type) {
	case map[string]any:
		if len(val) == 0 {
			buf.WriteString(sp + "<" + eName + "/>\n")
			return nil
		}
		buf.WriteString(sp + "<" + eName + ">\n")
		for _, k := range sortedKeys(val) {
			if err := xmlWriteValue(buf, k, val[k], indent+1); err != nil {
				return err
			}
		}
		buf.WriteString(sp + "</" + eName + ">\n")
	case []any:
		for _, item := range val {
			if err := xmlWriteValue(buf, "item", item, indent); err != nil {
				return err
			}
		}
	case nil:
		buf.WriteString(sp + "<" + eName + " xsi:nil=\"true\" xmlns:xsi=\"http://www.w3.org/2001/XMLSchema-instance\"/>\n")
	case string:
		buf.WriteString(sp + "<" + eName + ">" + xmlEscapeText(val) + "</" + eName + ">\n")
	case bool:
		buf.WriteString(sp + "<" + eName + ">" + strconv.FormatBool(val) + "</" + eName + ">\n")
	case float64:
		buf.WriteString(sp + "<" + eName + ">" + strconv.FormatFloat(val, 'f', -1, 64) + "</" + eName + ">\n")
	case int:
		buf.WriteString(sp + "<" + eName + ">" + strconv.Itoa(val) + "</" + eName + ">\n")
	default:
		out, err := json.Marshal(val)
		if err != nil {
			return err
		}
		buf.WriteString(sp + "<" + eName + ">" + xmlEscapeText(string(out)) + "</" + eName + ">\n")
	}
	return nil
}

// xmlUnmarshal parse un XML produit par ce paquet en map[string]any.
func xmlUnmarshal(body []byte) (any, error) {
	dec := xml.NewDecoder(bytes.NewReader(body))
	// Tolérer l'absence de racine unique en enveloppant.
	wrapped := bytes.NewBuffer(append([]byte("<wrapped>"), append(body, []byte("</wrapped>")...)...))
	dec = xml.NewDecoder(wrapped)
	return xmlParseValue(dec)
}

func xmlParseValue(dec *xml.Decoder) (any, error) {
	for {
		tok, err := dec.Token()
		if err != nil {
			if err == io.EOF {
				return nil, nil
			}
			return nil, err
		}
		switch se := tok.(type) {
		case xml.StartElement:
			return xmlParseElement(dec, se)
		}
	}
}

func xmlParseElement(dec *xml.Decoder, start xml.StartElement) (any, error) {
	obj := map[string]any{}
	hasText := false
	var textBuf strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			child, err := xmlParseElement(dec, t)
			if err != nil {
				return nil, err
			}
			name := t.Name.Local
			existing, ok := obj[name]
			if !ok {
				obj[name] = child
			} else {
				switch arr := existing.(type) {
				case []any:
					obj[name] = append(arr, child)
				default:
					obj[name] = []any{existing, child}
				}
			}
		case xml.CharData:
			text := strings.TrimSpace(string(t.Copy()))
			if text != "" {
				textBuf.WriteString(text)
				hasText = true
			}
		case xml.EndElement:
			if t.Name.Local == start.Name.Local {
				if len(obj) == 0 {
					return parseScalar(strings.TrimSpace(textBuf.String())), nil
				}
				if hasText {
					obj["_text"] = strings.TrimSpace(textBuf.String())
				}
				return obj, nil
			}
		}
	}
	return obj, nil
}

// xmlEscape échappe un nom d'élément XML (basique).
func xmlEscape(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch r {
		case '<':
			b.WriteString(`&lt;`)
		case '>':
			b.WriteString(`&gt;`)
		case '&':
			b.WriteString(`&amp;`)
		case '"':
			b.WriteString(`&quot;`)
		case '\'':
			b.WriteString(`&apos;`)
		default:
			if r < 0x20 {
				continue
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

// xmlEscapeText échappe le contenu texte XML.
func xmlEscapeText(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	s = strings.ReplaceAll(s, "'", "&apos;")
	return s
}

// encodeBase64 encode en base64 standard.
func encodeBase64(data []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var b strings.Builder
	for i := 0; i < len(data); i += 3 {
		chunk := data[i:]
		var n uint32
		l := len(chunk)
		if l > 0 {
			n |= uint32(chunk[0]) << 16
		}
		if l > 1 {
			n |= uint32(chunk[1]) << 8
		}
		if l > 2 {
			n |= uint32(chunk[2])
		}
		b.WriteByte(alphabet[n>>18&0x3F])
		b.WriteByte(alphabet[n>>12&0x3F])
		if l > 1 {
			b.WriteByte(alphabet[n>>6&0x3F])
		} else {
			b.WriteByte('=')
		}
		if l > 2 {
			b.WriteByte(alphabet[n&0x3F])
		} else {
			b.WriteByte('=')
		}
	}
	return b.String()
}
