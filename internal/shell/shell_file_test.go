package shell

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"crl-cli/internal/mint"
	"crl-cli/internal/request"
)

func newFileTestShell(t *testing.T) (*Shell, *strings.Builder, *mint.Manager, *mint.History, *request.Builder) {
	t.Helper()
	hist := mint.NewHistory(mint.DefaultHistoryConfig())
	mgr, err := mint.NewManager(mint.DefaultConfig(), hist)
	if err != nil {
		t.Fatal(err)
	}
	builder := request.New()
	var out strings.Builder
	sh, err := New(Options{
		In:      strings.NewReader(""),
		Out:     &out,
		Builder: builder,
		Manager: mgr,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sh.Close)
	return sh, &out, mgr, hist, builder
}

func TestCrlLsListsFilesAndDirectories(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sous-dossier"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.crl"), []byte("get /"), 0o644); err != nil {
		t.Fatal(err)
	}
	sh, out, _, _, _ := newFileTestShell(t)
	sh.Execute("crl.ls " + dir)
	got := out.String()
	if !strings.Contains(got, "sous-dossier") || !strings.Contains(got, "dir") {
		t.Fatalf("attendu un dossier avec le type dir :\n%s", got)
	}
	if !strings.Contains(got, "a.crl") || !strings.Contains(got, "file") {
		t.Fatalf("attendu un fichier avec le type file :\n%s", got)
	}
}

func TestCrlLsPathWithSpaces(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mon dossier")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.crl"), []byte("get /"), 0o644); err != nil {
		t.Fatal(err)
	}
	sh, out, _, _, _ := newFileTestShell(t)
	sh.Execute("crl.ls " + dir)
	if !strings.Contains(out.String(), "f.crl") {
		t.Fatalf("chemin avec espaces non listé :\n%s", out.String())
	}
}

func TestCrlMkdirCreatesDirectoryAndParents(t *testing.T) {
	base := t.TempDir()
	sh, out, _, _, _ := newFileTestShell(t)
	target := filepath.Join(base, "a", "b", "c")
	sh.Execute("crl.mkdir " + target)
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("dossier imbriqué non créé : %v", err)
	}
	if !strings.Contains(out.String(), "[CRL] Répertoire créé") {
		t.Fatalf("confirmation de création absente :\n%s", out.String())
	}
}

func TestCrlTouchCreatesEmptyFile(t *testing.T) {
	base := t.TempDir()
	sh, _, _, _, _ := newFileTestShell(t)
	target := filepath.Join(base, "f.crl")
	sh.Execute("crl.touch " + target)
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("fichier non créé : %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("fichier non vide : %d octets", info.Size())
	}
	// Idempotent : un second touch ne doit pas échouer.
	sh.Execute("crl.touch " + target)
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("fichier disparu après second touch : %v", err)
	}
}

func TestCrlRmRemovesFileWithYes(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "f.crl")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sh, _, _, _, _ := newFileTestShell(t)
	sh.Execute("crl.rm " + target + " --yes")
	if _, err := os.Stat(target); err == nil {
		t.Fatal("fichier toujours présent après crl.rm --yes")
	}
}

func TestCrlRmRefusesWithoutYesInScriptMode(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "f.crl")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sh, _, _, _, _ := newFileTestShell(t)
	sh.SetScriptMode(true)
	sh.Execute("crl.rm " + target)
	// En mode script, le diagnostic va sur stderr (errOut) ; le refus se
	// vérifie comportementalement : le fichier doit subsister.
	if _, err := os.Stat(target); err != nil {
		t.Fatal("fichier supprimé alors que le mode script devait refuser")
	}
}

func TestCrlRmAlias(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "f.crl")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sh, _, _, _, _ := newFileTestShell(t)
	sh.Execute("rm " + target + " --yes")
	if _, err := os.Stat(target); err == nil {
		t.Fatal("fichier toujours présent après rm --yes (alias)")
	}
}

func TestCrlRmdirRemovesEmptyDir(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "d")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	sh, _, _, _, _ := newFileTestShell(t)
	sh.Execute("crl.rmdir " + target + " --yes")
	if _, err := os.Stat(target); err == nil {
		t.Fatal("dossier toujours présent après crl.rmdir --yes")
	}
}

func TestCrlRmdirRefusesNonEmptyDirWithoutRecursive(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "d")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "f.crl"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sh, out, _, _, _ := newFileTestShell(t)
	sh.Execute("crl.rmdir " + target + " --yes")
	if _, err := os.Stat(target); err != nil {
		t.Fatal("dossier non vide supprimé sans --recursive")
	}
	if !strings.Contains(out.String(), "Erreur") {
		t.Fatalf("erreur attendue pour dossier non vide :\n%s", out.String())
	}
}

func TestCrlRmdirRecursiveRemovesNonEmptyDir(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "d")
	if err := os.MkdirAll(filepath.Join(target, "sous"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "f.crl"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sh, _, _, _, _ := newFileTestShell(t)
	sh.Execute("crl.rmdir " + target + " --recursive --yes")
	if _, err := os.Stat(target); err == nil {
		t.Fatal("dossier toujours présent après crl.rmdir --recursive --yes")
	}
}

func TestCrlEditNoEditorExplicitError(t *testing.T) {
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
	t.Setenv("PATH", "/nonexistent-dir")
	sh, out, _, _, _ := newFileTestShell(t)
	sh.interactive = true
	target := filepath.Join(t.TempDir(), "f.crl")
	sh.cmdEdit(target)
	if !strings.Contains(out.String(), "aucun éditeur disponible") {
		t.Fatalf("erreur éditeur manquant absente :\n%s", out.String())
	}
}

func TestResolveEditorVISUALPrecedence(t *testing.T) {
	t.Setenv("VISUAL", "/mon/editeur/fictif")
	t.Setenv("EDITOR", "/un/autre/editeur")
	if got := resolveEditor(); got != "/mon/editeur/fictif" {
		t.Fatalf("VISUAL doit prendre le dessus, obtenu %q", got)
	}
}

func TestCrlScriptExécuteFichierDotCrl(t *testing.T) {
	base := t.TempDir()
	scriptsDir := filepath.Join(base, "scripts", "vampi")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	auth := filepath.Join(scriptsDir, "auth.crl")
	if err := os.WriteFile(auth, []byte("crl.host http://example.test/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sh, out, _, _, _ := newFileTestShell(t)
	sh.Execute("crl.touch " + auth)
	sh.Execute("crl.script --script " + auth)
	got := out.String()
	if !strings.Contains(got, "example.test") {
		t.Fatalf("fichier .crl non exécuté par crl.script --script :\n%s", got)
	}
}
