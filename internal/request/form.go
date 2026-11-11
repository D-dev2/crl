package request

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"mime"
	"net/textproto"
	"os"
	"strings"
)

// FormPart représente une partie d'un formulaire multipart/form-data.
type FormPart struct {
	Name        string
	Value       string
	FileName    string
	ContentType string
	FilePath    string
	IsFile      bool
	Data        []byte
}

// multipartForm décrit un formulaire multipart en cours de construction.
type multipartForm struct {
	parts    []FormPart
	boundary string
}

// Form retourne le formulaire multipart du constructeur, nil s'il n'en a pas.
func (b *Builder) Form() *multipartForm {
	return b.form
}

// generateBoundary crée un boundary valide pour multipart/form-data.
func generateBoundary() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// Fallback déterministe et valide en cas d'impossibilité.
		return "----CRLC0000000000000000"
	}
	return "----CRLC" + hex.EncodeToString(buf)
}

// BoundaryValue retourne le boundary du formulaire, en générant un nouveau
// si nécessaire.
func (f *multipartForm) BoundaryValue() string {
	if f.boundary == "" {
		f.boundary = generateBoundary()
	}
	return f.boundary
}

// addPart ajoute une partie au formulaire.
func (f *multipartForm) addPart(p FormPart) {
	f.parts = append(f.parts, p)
}

// removePart retire toutes les parties portant le nom donné. Retourne le
// nombre de parties retirées.
func (f *multipartForm) removePart(name string) int {
	out := f.parts[:0]
	removed := 0
	for _, p := range f.parts {
		if p.Name == name {
			removed++
			continue
		}
		out = append(out, p)
	}
	f.parts = out
	return removed
}

// list retourne une copie des parties.
func (f *multipartForm) list() []FormPart {
	out := make([]FormPart, len(f.parts))
	copy(out, f.parts)
	return out
}

// clear retire toutes les parties.
func (f *multipartForm) clear() {
	f.parts = nil
	f.boundary = ""
}

// IsMultipartContentType indique si un Content-Type est du multipart/form-data.
func IsMultipartContentType(contentType string) bool {
	ct, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return strings.EqualFold(contentType, "multipart/form-data")
	}
	return strings.EqualFold(ct, "multipart/form-data")
}

// ExtractBoundary retourne le boundary d'un Content-Type multipart, vide si
// absent. Utilisé pour l'affichage et l'inspection des captures.
func ExtractBoundary(contentType string) string {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	return params["boundary"]
}

// escapeQuote échappe les guillemets et anti-slashes pour un attribut quoted-string.
func escapeQuote(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// BuildMultipart construit le corps multipart/form-data à partir du formulaire
// et retourne le corps ainsi que le Content-Type complet avec boundary.
func (b *Builder) BuildMultipart() ([]byte, string) {
	f := b.form
	if f == nil {
		return nil, ""
	}
	boundary := f.BoundaryValue()
	var body bytes.Buffer
	for _, p := range f.parts {
		body.WriteString("--" + boundary + "\r\n")
		body.WriteString("Content-Disposition: form-data; name=\"" + escapeQuote(p.Name) + "\"")
		if p.FileName != "" {
			body.WriteString("; filename=\"" + escapeQuote(p.FileName) + "\"")
		}
		body.WriteString("\r\n")
		if p.ContentType != "" {
			body.WriteString("Content-Type: " + p.ContentType + "\r\n")
		}
		body.WriteString("\r\n")
		data := p.Data
		if !p.IsFile {
			data = []byte(p.Value)
		} else if len(data) == 0 && p.FilePath != "" {
			data, _ = os.ReadFile(p.FilePath)
		}
		body.Write(data)
		body.WriteString("\r\n")
	}
	body.WriteString("--" + boundary + "--\r\n")
	contentType := "multipart/form-data; boundary=" + boundary
	return body.Bytes(), contentType
}

// ParseMultipart découpe un corps multipart/form-data en parties (champs et
// fichiers) à partir du boundary contenu dans le Content-Type.
func ParseMultipart(body []byte, contentType string) ([]FormPart, error) {
	boundary := ExtractBoundary(contentType)
	if boundary == "" {
		return nil, fmt.Errorf("boundary introuvable dans %q", contentType)
	}
	sep := []byte("--" + boundary)
	var parts []FormPart
	rest := body
	for {
		idx := bytes.Index(rest, sep)
		if idx < 0 {
			break
		}
		rest = rest[idx+len(sep):]
		// Fin du multipart : "--boundary--"
		if len(rest) >= 2 && rest[0] == '-' && rest[1] == '-' {
			break
		}
		// Sauter le CRLF après le séparateur.
		if len(rest) >= 2 && rest[0] == '\r' && rest[1] == '\n' {
			rest = rest[2:]
		} else if len(rest) >= 1 && rest[0] == '\n' {
			rest = rest[1:]
		}
		// Fin des headers de la partie : ligne vide.
		headerEnd := bytes.Index(rest, []byte("\r\n\r\n"))
		if headerEnd < 0 {
			headerEnd = bytes.Index(rest, []byte("\n\n"))
			if headerEnd < 0 {
				break
			}
			headerBlock := rest[:headerEnd]
			rest = rest[headerEnd+2:]
			p := parsePartHeaders(headerBlock)
			// Le corps va jusqu'au prochain séparateur ou à la fin.
			next := bytes.Index(rest, sep)
			var partBody []byte
			if next < 0 {
				partBody = rest
				rest = nil
			} else {
				partBody = rest[:next]
				// Retirer le CRLF final avant le séparateur.
				if len(partBody) >= 2 && partBody[len(partBody)-2] == '\r' && partBody[len(partBody)-1] == '\n' {
					partBody = partBody[:len(partBody)-2]
				} else if len(partBody) >= 1 && partBody[len(partBody)-1] == '\n' {
					partBody = partBody[:len(partBody)-1]
				}
				rest = rest[next:]
			}
			p.Data = partBody
			parts = append(parts, p)
			if rest == nil {
				break
			}
			continue
		}
		headerBlock := rest[:headerEnd]
		rest = rest[headerEnd+4:]
		p := parsePartHeaders(headerBlock)
		next := bytes.Index(rest, sep)
		var partBody []byte
		if next < 0 {
			partBody = rest
			rest = nil
		} else {
			partBody = rest[:next]
			if len(partBody) >= 2 && partBody[len(partBody)-2] == '\r' && partBody[len(partBody)-1] == '\n' {
				partBody = partBody[:len(partBody)-2]
			} else if len(partBody) >= 1 && partBody[len(partBody)-1] == '\n' {
				partBody = partBody[:len(partBody)-1]
			}
			rest = rest[next:]
		}
		p.Data = partBody
		parts = append(parts, p)
		if rest == nil {
			break
		}
	}
	return parts, nil
}

// parsePartHeaders extrait les headers d'une partie multipart.
func parsePartHeaders(block []byte) FormPart {
	p := FormPart{}
	tp := textproto.NewReader(bufio.NewReader(bytes.NewReader(block)))
	headers, err := tp.ReadMIMEHeader()
	if err != nil && len(headers) == 0 {
		return p
	}
	disp := headers.Get("Content-Disposition")
	if disp != "" {
		_, params, _ := mime.ParseMediaType(disp)
		p.Name = params["name"]
		p.FileName = params["filename"]
	}
	p.ContentType = headers.Get("Content-Type")
	if p.FileName != "" {
		p.IsFile = true
	}
	return p
}
