package shell

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// jsonType désigne le type d'une valeur JSON ordonnée.
type jsonType int

const (
	jsonNull jsonType = iota
	jsonBool
	jsonNumber
	jsonString
	jsonArray
	jsonObject
	jsonRaw // texte JSON brut préservé exactement
)

// jsonValue est une valeur JSON dont l'ordre des clés d'objet et le texte brut
// des parties non modifiées sont préservés à la sérialisation.
type jsonValue struct {
	typ   jsonType
	raw   string // texte brut pour null, bool, number, string, raw
	keys  []string
	vals  []*jsonValue
	items []*jsonValue
}

// parseJSONOrdered décode data en préservant l'ordre des clés d'objet et le
// texte brut de chaque valeur. Une erreur de syntaxe retourne la position.
func parseJSONOrdered(data []byte) (*jsonValue, error) {
	p := &jsonOrderedParser{data: data, pos: 0}
	v, err := p.parseValue()
	if err != nil {
		return nil, err
	}
	p.skipWS()
	if p.pos < len(p.data) {
		return nil, fmt.Errorf("JSON invalide : caractère inattendu après la valeur à l'octet %d", p.pos)
	}
	return v, nil
}

type jsonOrderedParser struct {
	data []byte
	pos  int
}

func (p *jsonOrderedParser) skipWS() {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *jsonOrderedParser) parseValue() (*jsonValue, error) {
	p.skipWS()
	if p.pos >= len(p.data) {
		return nil, fmt.Errorf("JSON invalide : fin inattendue")
	}
	switch p.data[p.pos] {
	case '{':
		return p.parseObject()
	case '[':
		return p.parseArray()
	case '"':
		return p.parseString()
	case 't', 'f':
		return p.parseBool()
	case 'n':
		return p.parseNull()
	default:
		return p.parseNumber()
	}
}

func (p *jsonOrderedParser) parseObject() (*jsonValue, error) {
	v := &jsonValue{typ: jsonObject}
	p.pos++ // skip {
	p.skipWS()
	if p.pos < len(p.data) && p.data[p.pos] == '}' {
		p.pos++
		return v, nil
	}
	for {
		p.skipWS()
		key, err := p.parseString()
		if err != nil {
			return nil, err
		}
		p.skipWS()
		if p.pos >= len(p.data) || p.data[p.pos] != ':' {
			return nil, fmt.Errorf("JSON invalide : ':' attendu après la clé %q", key.raw)
		}
		p.pos++ // skip :
		val, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		v.keys = append(v.keys, key.raw)
		v.vals = append(v.vals, val)
		p.skipWS()
		if p.pos >= len(p.data) {
			return nil, fmt.Errorf("JSON invalide : fin inattendue dans l'objet")
		}
		if p.data[p.pos] == ',' {
			p.pos++
			continue
		}
		if p.data[p.pos] == '}' {
			p.pos++
			return v, nil
		}
		return nil, fmt.Errorf("JSON invalide : ',' ou '}' attendu à l'octet %d", p.pos)
	}
}

func (p *jsonOrderedParser) parseArray() (*jsonValue, error) {
	v := &jsonValue{typ: jsonArray}
	p.pos++ // skip [
	p.skipWS()
	if p.pos < len(p.data) && p.data[p.pos] == ']' {
		p.pos++
		return v, nil
	}
	for {
		item, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		v.items = append(v.items, item)
		p.skipWS()
		if p.pos >= len(p.data) {
			return nil, fmt.Errorf("JSON invalide : fin inattendue dans le tableau")
		}
		if p.data[p.pos] == ',' {
			p.pos++
			continue
		}
		if p.data[p.pos] == ']' {
			p.pos++
			return v, nil
		}
		return nil, fmt.Errorf("JSON invalide : ',' ou ']' attendu à l'octet %d", p.pos)
	}
}

func (p *jsonOrderedParser) parseString() (*jsonValue, error) {
	if p.data[p.pos] != '"' {
		return nil, fmt.Errorf("JSON invalide : chaîne attendue")
	}
	p.pos++ // skip opening quote
	var sb strings.Builder
	for p.pos < len(p.data) {
		c := p.data[p.pos]
		if c == '"' {
			p.pos++ // skip closing quote
			return &jsonValue{typ: jsonString, raw: sb.String()}, nil
		}
		if c == '\\' {
			p.pos++
			if p.pos >= len(p.data) {
				return nil, fmt.Errorf("JSON invalide : séquence d'échappement incomplète")
			}
			switch p.data[p.pos] {
			case '"':
				sb.WriteByte('"')
			case '\\':
				sb.WriteByte('\\')
			case '/':
				sb.WriteByte('/')
			case 'b':
				sb.WriteByte('\b')
			case 'f':
				sb.WriteByte('\f')
			case 'n':
				sb.WriteByte('\n')
			case 'r':
				sb.WriteByte('\r')
			case 't':
				sb.WriteByte('\t')
			case 'u':
				if p.pos+4 >= len(p.data) {
					return nil, fmt.Errorf("JSON invalide : séquence unicode incomplète")
				}
				p.pos++
				hex := string(p.data[p.pos : p.pos+4])
				code, err := strconv.ParseUint(hex, 16, 16)
				if err != nil {
					return nil, fmt.Errorf("JSON invalide : séquence unicode invalide")
				}
				sb.WriteRune(rune(code))
				p.pos += 3
			default:
				return nil, fmt.Errorf("JSON invalide : échappement inconnu \\%c", p.data[p.pos])
			}
			p.pos++
			continue
		}
		sb.WriteByte(c)
		p.pos++
	}
	return nil, fmt.Errorf("JSON invalide : chaîne non terminée")
}

func (p *jsonOrderedParser) parseBool() (*jsonValue, error) {
	if strings.HasPrefix(string(p.data[p.pos:]), "true") {
		p.pos += 4
		return &jsonValue{typ: jsonBool, raw: "true"}, nil
	}
	if strings.HasPrefix(string(p.data[p.pos:]), "false") {
		p.pos += 5
		return &jsonValue{typ: jsonBool, raw: "false"}, nil
	}
	return nil, fmt.Errorf("JSON invalide : valeur booléenne attendue à l'octet %d", p.pos)
}

func (p *jsonOrderedParser) parseNull() (*jsonValue, error) {
	if strings.HasPrefix(string(p.data[p.pos:]), "null") {
		p.pos += 4
		return &jsonValue{typ: jsonNull, raw: "null"}, nil
	}
	return nil, fmt.Errorf("JSON invalide : valeur nulle attendue à l'octet %d", p.pos)
}

func (p *jsonOrderedParser) parseNumber() (*jsonValue, error) {
	start := p.pos
	if p.data[p.pos] == '-' {
		p.pos++
	}
	for p.pos < len(p.data) && (p.data[p.pos] >= '0' && p.data[p.pos] <= '9') {
		p.pos++
	}
	if p.pos < len(p.data) && p.data[p.pos] == '.' {
		p.pos++
		for p.pos < len(p.data) && (p.data[p.pos] >= '0' && p.data[p.pos] <= '9') {
			p.pos++
		}
	}
	if p.pos < len(p.data) && (p.data[p.pos] == 'e' || p.data[p.pos] == 'E') {
		p.pos++
		if p.pos < len(p.data) && (p.data[p.pos] == '+' || p.data[p.pos] == '-') {
			p.pos++
		}
		for p.pos < len(p.data) && (p.data[p.pos] >= '0' && p.data[p.pos] <= '9') {
			p.pos++
		}
	}
	raw := string(p.data[start:p.pos])
	if _, err := strconv.ParseFloat(raw, 64); err != nil {
		return nil, fmt.Errorf("JSON invalide : nombre invalide %q", raw)
	}
	return &jsonValue{typ: jsonNumber, raw: raw}, nil
}

// MarshalJSON restitue la valeur en préservant l'ordre des clés et le texte
// brut des parties non resérialisées.
func (v *jsonValue) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	if err := v.writeJSON(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (v *jsonValue) writeJSON(buf *bytes.Buffer) error {
	switch v.typ {
	case jsonRaw, jsonNumber, jsonBool, jsonNull:
		buf.WriteString(v.raw)
	case jsonString:
		if err := writeJSONString(buf, v.raw); err != nil {
			return err
		}
	case jsonArray:
		buf.WriteByte('[')
		for i, item := range v.items {
			if i > 0 {
				buf.WriteByte(',')
				buf.WriteByte(' ')
			}
			if err := item.writeJSON(buf); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case jsonObject:
		buf.WriteByte('{')
		for i, key := range v.keys {
			if i > 0 {
				buf.WriteByte(',')
				buf.WriteByte(' ')
			}
			if err := writeJSONString(buf, key); err != nil {
				return err
			}
			buf.WriteByte(':')
			buf.WriteByte(' ')
			if err := v.vals[i].writeJSON(buf); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("type JSON inconnu %d", v.typ)
	}
	return nil
}

func writeJSONString(buf *bytes.Buffer, s string) error {
	buf.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if size == 0 {
			buf.WriteByte(s[i])
			i++
			continue
		}
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(buf, `\u%04x`, r)
			} else {
				buf.WriteString(s[i : i+size])
			}
		}
		i += size
	}
	buf.WriteByte('"')
	return nil
}

// jsonSet définit la valeur à l'emplacement donné par parts (pointeur JSON
// ou notation pointée). Les parties intermédiaires sont créées si nécessaire.
func (v *jsonValue) jsonSet(parts []string, value *jsonValue) error {
	if len(parts) == 0 {
		*v = *value
		return nil
	}
	if v.typ != jsonObject {
		*v = jsonValue{typ: jsonObject}
	}
	for i, key := range parts {
		idx := -1
		for j, k := range v.keys {
			if k == key {
				idx = j
				break
			}
		}
		if i == len(parts)-1 {
			if idx >= 0 {
				v.vals[idx] = value
			} else {
				v.keys = append(v.keys, key)
				v.vals = append(v.vals, value)
			}
			return nil
		}
		if idx < 0 {
			child := &jsonValue{typ: jsonObject}
			v.keys = append(v.keys, key)
			v.vals = append(v.vals, child)
			idx = len(v.vals) - 1
		}
		v = v.vals[idx]
	}
	return nil
}

// jsonDelete supprime la clé à l'emplacement donné par parts.
func (v *jsonValue) jsonDelete(parts []string) error {
	if len(parts) == 0 {
		return fmt.Errorf("chemin JSON vide")
	}
	if v.typ != jsonObject {
		return fmt.Errorf("chemin %q introuvable", parts[0])
	}
	idx := -1
	for j, k := range v.keys {
		if k == parts[0] {
			idx = j
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("propriété %q absente", parts[0])
	}
	if len(parts) == 1 {
		v.keys = append(v.keys[:idx], v.keys[idx+1:]...)
		v.vals = append(v.vals[:idx], v.vals[idx+1:]...)
		return nil
	}
	return v.vals[idx].jsonDelete(parts[1:])
}

// jsonGet retourne la valeur à l'emplacement donné par parts.
func (v *jsonValue) jsonGet(parts []string) (*jsonValue, bool) {
	if len(parts) == 0 {
		return v, true
	}
	if v.typ != jsonObject {
		return nil, false
	}
	for j, k := range v.keys {
		if k == parts[0] {
			return v.vals[j].jsonGet(parts[1:])
		}
	}
	return nil, false
}

// makeJSONRaw crée une valeur JSON à partir d'un texte brut déjà valide.
func makeJSONRaw(raw string) *jsonValue {
	return &jsonValue{typ: jsonRaw, raw: raw}
}

// makeJSONString crée une valeur JSON chaîne (le contenu est stocké sans
// guillemets ; writeJSONString les ajoute à la sérialisation).
func makeJSONString(s string) *jsonValue {
	return &jsonValue{typ: jsonString, raw: s}
}

// makeJSONNumber crée une valeur JSON nombre.
func makeJSONNumber(s string) (*jsonValue, error) {
	if _, err := strconv.ParseFloat(s, 64); err != nil {
		return nil, fmt.Errorf("nombre JSON invalide %q", s)
	}
	return &jsonValue{typ: jsonNumber, raw: s}, nil
}

// jsonPathTokens découpe un chemin JSON Pointer (RFC 6902) ou pointé en jetons.
func jsonPathTokens(expression string) ([]string, error) {
	expression = strings.TrimSpace(expression)
	if expression == "" {
		return nil, fmt.Errorf("chemin JSON vide")
	}
	if strings.HasPrefix(expression, "/") {
		return jsonPointerTokens(expression)
	}
	return strings.Split(expression, "."), nil
}

// updateOrderedJSON applique une opération set/delete sur un JSON brut en
// préservant exactement l'ordre des clés et le texte des parties non touchées.
// Pour une opération "set", value contient déjà la valeur JSON à insérer.
func updateOrderedJSON(body []byte, operation, path string, value *jsonValue) ([]byte, error) {
	root, err := parseJSONOrdered(body)
	if err != nil {
		return nil, fmt.Errorf("JSON existant invalide: %w", err)
	}
	parts, err := jsonPathTokens(path)
	if err != nil {
		return nil, err
	}
	if operation == "set" {
		if err := root.jsonSet(parts, value); err != nil {
			return nil, err
		}
	} else {
		if err := root.jsonDelete(parts); err != nil {
			return nil, err
		}
	}
	return root.MarshalJSON()
}
