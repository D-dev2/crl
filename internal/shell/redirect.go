package shell

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"crl-cli/internal/display"
	"crl-cli/internal/request"
	"crl-cli/internal/response"
)

// httpHop décrit une réponse HTTP individuelle, y compris les redirections
// intermédiaires. Les corps intermédiaires sont conservés en mémoire pour
// l'affichage ; seul le dernier corps reste sur disque pour la consultation
// complète.
type httpHop struct {
	request  *request.Builder
	status   response.Status
	headers  response.Headers
	meta     response.CurlMeta
	body     []byte
	total    int64
	bodyPath string
	failure  *response.CurlFailure
	err      error

	location     string
	nextURL      string
	nextMethod   string
	nextBodyKept bool
}

type httpExchange struct {
	initialURL  string
	hops        []httpHop
	redirectErr error
}

func (e httpExchange) final() (httpHop, bool) {
	if len(e.hops) == 0 {
		return httpHop{}, false
	}
	return e.hops[len(e.hops)-1], true
}

// runHTTPWithRedirects exécute une requête puis, si le suivi est actif, chaque
// redirection successivement. Chaque saut est une invocation curl distincte :
// cela permet d'afficher chaque statut, de respecter les règles de méthode et
// de corps, et d'appliquer les restrictions cross-origin avant de poursuivre.
func (s *Shell) runHTTPWithRedirects(ctx context.Context, start *request.Builder, res request.ProxyResolution, dir string) httpExchange {
	exchange := httpExchange{initialURL: start.URL()}
	headerPath := filepath.Join(dir, "headers.txt")
	bodyPath := filepath.Join(dir, "body.bin")
	current := start.Clone()
	redirectsFollowed := 0
	visited := map[string]bool{request.RedirectKey(start.URL()): true}

	for {
		if err := os.WriteFile(headerPath, nil, 0o600); err != nil {
			exchange.hops = append(exchange.hops, httpHop{request: current, err: fmt.Errorf("préparation des en-têtes : %w", err)})
			return exchange
		}
		if err := os.WriteFile(bodyPath, nil, 0o600); err != nil {
			exchange.hops = append(exchange.hops, httpHop{request: current, err: fmt.Errorf("préparation du corps : %w", err)})
			return exchange
		}

		args, err := current.CurlArgs(res)
		if err != nil {
			exchange.hops = append(exchange.hops, httpHop{request: current, err: err})
			return exchange
		}
		args = append(args, "--dump-header", headerPath, "--output", bodyPath, "--write-out", response.WriteOutFormat)

		cmd := exec.CommandContext(ctx, "curl", args...)
		if current.BodySet() {
			cmd.Stdin = current.BodyReader()
		}
		var metaOut, stderr bytes.Buffer
		cmd.Stdout = &metaOut
		cmd.Stderr = &stderr
		started := time.Now()
		runErr := cmd.Run()
		elapsed := time.Since(started)

		headerData, _ := os.ReadFile(headerPath)
		status, headers := response.ParseHeaderDump(headerData)
		meta := response.ParseCurlMeta(metaOut.Bytes())
		if meta.Duration <= 0 {
			meta.Duration = elapsed
		}
		var total int64
		if info, statErr := os.Stat(bodyPath); statErr == nil {
			total = info.Size()
		}
		bodyPreview := readFilePrefix(bodyPath, launchBodyLimit)
		hop := httpHop{
			request:  current,
			status:   status,
			headers:  headers,
			meta:     meta,
			body:     bodyPreview,
			total:    total,
			bodyPath: bodyPath,
		}

		if runErr != nil {
			failure := response.ClassifyCurlFailure(curlExitCode(runErr), stderr.String())
			if ctxErr := ctx.Err(); ctxErr != nil {
				if ctxErr == context.DeadlineExceeded {
					failure.Label = "délai dépassé"
				} else {
					failure.Label = "interrompu"
				}
				if failure.Detail == "" {
					failure.Detail = ctxErr.Error()
				} else if !strings.Contains(failure.Detail, ctxErr.Error()) {
					failure.Detail += " : " + ctxErr.Error()
				}
			}
			hop.failure = &failure
			exchange.hops = append(exchange.hops, hop)
			return exchange
		}
		if status.Code == 0 {
			hop.err = fmt.Errorf("curl n'a renvoyé aucune ligne de statut HTTP exploitable")
			exchange.hops = append(exchange.hops, hop)
			return exchange
		}

		exchange.hops = append(exchange.hops, hop)
		index := len(exchange.hops) - 1

		if !s.followRedirects || !request.FollowableRedirect(status.Code) {
			return exchange
		}
		location := headers.Get("Location")
		if strings.TrimSpace(location) == "" {
			return exchange
		}
		// La destination est affichée même si elle ne peut pas être suivie.
		exchange.hops[index].location = location
		if redirectsFollowed >= request.DefaultMaxRedirects {
			exchange.redirectErr = fmt.Errorf("limite de redirections atteinte (%d) : boucle probable", request.DefaultMaxRedirects)
			return exchange
		}

		nextURL, err := request.ResolveRedirect(current.URL(), location)
		if err != nil {
			exchange.redirectErr = fmt.Errorf("redirection %d invalide : %w", redirectsFollowed+1, err)
			return exchange
		}
		exchange.hops[index].nextURL = nextURL
		if visited[request.RedirectKey(nextURL)] {
			exchange.redirectErr = fmt.Errorf("boucle de redirection détectée vers %s", nextURL)
			return exchange
		}
		visited[request.RedirectKey(nextURL)] = true
		nextMethod, keepBody := request.RedirectMethodBody(status.Code, current.Method())
		next := current.Clone()
		if err := next.SetMethod(nextMethod); err != nil {
			exchange.redirectErr = fmt.Errorf("méthode de redirection invalide : %w", err)
			return exchange
		}
		if err := next.SetURL(nextURL); err != nil {
			exchange.redirectErr = fmt.Errorf("URL de redirection invalide : %w", err)
			return exchange
		}
		if !keepBody {
			next.ClearBody()
			request.DropBodyHeaders(next)
		}
		crossOrigin := !request.SameOrigin(current.URL(), nextURL)
		if crossOrigin && !s.redirectCredentials {
			request.StripCrossOriginCredentials(next)
		}

		exchange.hops[index].nextMethod = nextMethod
		exchange.hops[index].nextBodyKept = keepBody
		current = next
		redirectsFollowed++
	}
}

// hopDisplay regroupe les données communes au rendu d'une réponse
// intermédiaire, qu'elle vienne de crl.launch ou de mint.replay.
type hopDisplay struct {
	title        string
	method       string
	url          string
	status       response.Status
	headers      response.Headers
	body         []byte
	total        int64
	location     string
	nextURL      string
	nextMethod   string
	nextBodyKept bool
}

// printRedirectHop affiche une réponse intermédiaire de redirection avec ses
// en-têtes et son corps, sans la confondre avec la réponse finale.
func (s *Shell) printRedirectHop(number int, hop httpHop) {
	label := "RÉPONSE INITIALE"
	if number > 1 {
		label = fmt.Sprintf("RÉPONSE INTERMÉDIAIRE %d", number)
	}
	s.printHopBlock(hopDisplay{
		title:        label + " · " + strings.ToUpper(sanitizeTerminal(hop.status.Label())),
		method:       hop.request.Method(),
		url:          hop.request.URL(),
		status:       hop.status,
		headers:      hop.headers,
		body:         hop.body,
		total:        hop.total,
		location:     hop.location,
		nextURL:      hop.nextURL,
		nextMethod:   hop.nextMethod,
		nextBodyKept: hop.nextBodyKept,
	})
}

// printFinalHop affiche la réponse finale d'une chaîne de redirections dans un
// cadre propre, sans remplacer les réponses intermédiaires déjà affichées.
func (s *Shell) printFinalHop(hop httpHop) {
	s.printHopBlock(hopDisplay{
		title:        "RÉPONSE FINALE · " + strings.ToUpper(sanitizeTerminal(hop.status.Label())),
		method:       hop.request.Method(),
		url:          hop.request.URL(),
		status:       hop.status,
		headers:      hop.headers,
		body:         hop.body,
		total:        hop.total,
		location:     hop.location,
		nextURL:      hop.nextURL,
		nextMethod:   hop.nextMethod,
		nextBodyKept: hop.nextBodyKept,
	})
}

// printHopBlock met en évidence une réponse HTTP complète dans un cadre. Les
// en-têtes et le corps sont affichés pour cette étape précise, sans remplacer
// la réponse initiale par la destination.
func (s *Shell) printHopBlock(hop hopDisplay) {
	p := s.pal
	width := s.boxWidth()
	title := " " + hop.title + " "
	top := "╭─" + title
	top += strings.Repeat("─", max(0, width-display.VisibleWidth(top)-1)) + "╮"
	bottom := "╰" + strings.Repeat("─", max(0, width-2)) + "╯"
	s.printf("%s\n", p.BoldCyan(top))
	s.replayField("Méthode", p.Method(sanitizeTerminal(hop.method)), width)
	s.replayField("URL", p.Cyan(sanitizeTerminal(hop.url)), width)
	if hop.location != "" {
		s.replayField("Location", p.Cyan(sanitizeTerminal(hop.location)), width)
	}
	if hop.nextURL != "" {
		s.replayField("URL suivante", p.Cyan(sanitizeTerminal(hop.nextURL)), width)
	}
	if hop.nextMethod != "" {
		detail := "corps conservé"
		if !hop.nextBodyKept {
			detail = "corps abandonné"
		}
		s.replayField("Méthode suivante", p.Method(hop.nextMethod)+" "+p.Dim("· "+detail), width)
	}

	s.printf("│\n│ %s\n", p.BoldBlue(fmt.Sprintf("En-têtes de réponse (%d)", len(hop.headers))))
	if len(hop.headers) == 0 {
		s.printf("│   %s\n", p.Dim("(aucun)"))
	} else {
		for _, h := range hop.headers {
			s.printf("%s\n", s.headerLine(h.Name, h.Value, "│   "))
		}
	}

	view := response.RenderBody(hop.body, hop.total, launchBodyLimit, s.outputMode, hop.headers.ContentType())
	s.printf("│\n│ %s\n", p.BoldBlue(fmt.Sprintf("Corps de la réponse — %s (%s)", bodyKindLabel(view.Kind), display.FormatBytes(hop.total))))
	bodyWidth := max(10, width-4)
	for _, line := range strings.Split(view.Text, "\n") {
		for _, wrapped := range display.WrapText(sanitizeTerminal(line), bodyWidth) {
			s.printf("│   %s\n", wrapped)
		}
	}
	if view.Truncated {
		s.printf("│   %s\n", p.BoldYellow(fmt.Sprintf("[…] Affichage limité à %s sur %s reçus ; le corps complet de la réponse finale reste disponible.", display.FormatBytes(launchBodyLimit), display.FormatBytes(hop.total))))
	}
	s.printf("%s\n", p.BoldCyan(bottom))
}

// printRedirectArrow sépare visuellement deux réponses d'une chaîne.
func (s *Shell) printRedirectArrow() {
	s.printf("\n%s\n\n", s.pal.BoldCyan("             ↓ REDIRECTION"))
}

// printRedirectChain résume l'ordre des redirections suivies.
func (s *Shell) printRedirectChain(exchange httpExchange) {
	if len(exchange.hops) <= 1 {
		return
	}
	p := s.pal
	intermediate := exchange.hops[:len(exchange.hops)-1]
	s.printf("\n%s\n", p.BoldBlue(fmt.Sprintf("Chaîne de redirections (%d)", len(intermediate))))
	for i, hop := range intermediate {
		next := hop.nextURL
		if next == "" {
			next = "(destination inconnue)"
		}
		s.printf("  %d. %s · %s → %s\n", i+1,
			p.StatusCode(hop.status.Code, sanitizeTerminal(hop.status.Label())),
			p.Cyan(sanitizeTerminal(hop.request.URL())),
			p.Cyan(sanitizeTerminal(next)))
	}
	if final, ok := exchange.final(); ok {
		s.printf("  %s %s\n", p.Dim("URL finale :"), p.Cyan(sanitizeTerminal(final.request.URL())))
	}
}

// replayRedirect conserve les informations d'une réponse intermédiaire pour le
// rendu différé de mint.replay.
type replayRedirect struct {
	status       response.Status
	method       string
	url          string
	location     string
	nextURL      string
	nextMethod   string
	nextBodyKept bool
	headers      response.Headers
	body         []byte
	total        int64
}

// replayRedirects convertit les sauts intermédiaires d'un échange en données
// conservables jusqu'à l'affichage du lot.
func replayRedirects(exchange httpExchange) []replayRedirect {
	if len(exchange.hops) <= 1 {
		return nil
	}
	out := make([]replayRedirect, 0, len(exchange.hops)-1)
	for _, hop := range exchange.hops[:len(exchange.hops)-1] {
		out = append(out, replayRedirect{
			status:       hop.status,
			method:       hop.request.Method(),
			url:          hop.request.URL(),
			location:     hop.location,
			nextURL:      hop.nextURL,
			nextMethod:   hop.nextMethod,
			nextBodyKept: hop.nextBodyKept,
			headers:      hop.headers,
			body:         append([]byte(nil), hop.body...),
			total:        hop.total,
		})
	}
	return out
}

// printReplayRedirects affiche chaque réponse intermédiaire du rejeu avant la
// réponse finale, dans l'ordre réel de la chaîne.
func (s *Shell) printReplayRedirects(r replayResult) {
	if len(r.redirects) == 0 {
		return
	}
	for i, hop := range r.redirects {
		label := "RÉPONSE INITIALE"
		if i > 0 {
			label = fmt.Sprintf("RÉPONSE INTERMÉDIAIRE %d", i+1)
		}
		s.printHopBlock(hopDisplay{
			title:        label + " · " + strings.ToUpper(sanitizeTerminal(hop.status.Label())),
			method:       hop.method,
			url:          hop.url,
			status:       hop.status,
			headers:      hop.headers,
			body:         hop.body,
			total:        hop.total,
			location:     hop.location,
			nextURL:      hop.nextURL,
			nextMethod:   hop.nextMethod,
			nextBodyKept: hop.nextBodyKept,
		})
		s.printRedirectArrow()
	}
}
