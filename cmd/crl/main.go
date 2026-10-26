package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"crl-cli/internal/mint"
	"crl-cli/internal/request"
	"crl-cli/internal/shell"
	"crl-cli/internal/termstyle"
)

const version = "0.3.1"

func main() {
	showVersion := flag.Bool("version", false, "afficher la version")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "CRL-CLI %s — constructeur et rejeu de requêtes HTTP avec curl et Mint\n\n", version)
		fmt.Fprintf(flag.CommandLine.Output(), "Usage : crl [options]\n\n")
		flag.PrintDefaults()
		fmt.Fprintf(flag.CommandLine.Output(), "\nVariables d'environnement Mint :\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  MINT_PORT              port d'écoute (défaut 8080)\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  MINT_MAX_BODY_BYTES    taille max d'un corps conservé\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  MINT_MAX_CAPTURES      nombre max de captures\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  MINT_MAX_TOTAL_BYTES   mémoire max des corps\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  MINT_MITMDUMP          chemin explicite de mitmdump\n\n")
		fmt.Fprintf(flag.CommandLine.Output(), "Exemples :\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  crl\n")
		fmt.Fprintf(flag.CommandLine.Output(), "  MINT_PORT=8081 crl\n")
	}
	flag.Parse()

	if *showVersion {
		fmt.Printf("crl-cli %s\n", version)
		return
	}

	cfg := mint.DefaultConfig()
	if err := applyEnvironment(&cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Configuration Mint invalide : %v\n", err)
		os.Exit(2)
	}

	history := mint.NewHistory(mint.HistoryConfig{
		MaxCaptures:       cfg.MaxCaptures,
		MaxBodyBytes:      cfg.MaxBodyBytes,
		MaxTotalBodyBytes: cfg.MaxTotalBodyBytes,
	})
	manager, err := mint.NewManager(cfg, history)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Initialisation de Mint : %v\n", err)
		os.Exit(2)
	}

	notifications := make(chan mint.Notification, 1024)
	manager.SetNotifier(func(n mint.Notification) {
		select {
		case notifications <- n:
		default:
			// Une notification peut être perdue si le shell est très occupé ;
			// la capture reste enregistrée dans l'historique.
		}
	})

	sh, err := shell.New(shell.Options{
		In:            os.Stdin,
		Out:           os.Stdout,
		Builder:       request.New(),
		Manager:       manager,
		Notifications: notifications,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Initialisation du shell : %v\n", err)
		os.Exit(2)
	}

	fmt.Printf("%s\n", termstyle.New(os.Stdout).BoldCyan("CRL-CLI "+version+" — constructeur de requêtes HTTP (curl)"))
	fmt.Println("Tapez help pour les commandes CRL, mint.help pour Mint.")
	if _, err := manager.ResolveMitmdump(); err != nil {
		fmt.Println("Mint : mitmdump n'est pas détecté. Installez mitmproxy pour activer la capture.")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	defer manager.Close()
	defer sh.Close()

	// Si le shell est bloqué sur une entrée non interactive, un signal doit
	// tout de même arrêter le proxy lancé par l'application.
	go func() {
		<-ctx.Done()
		_ = manager.Stop(context.Background())
	}()

	if err := sh.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Erreur shell : %v\n", err)
		os.Exit(1)
	}
}

func applyEnvironment(cfg *mint.Config) error {
	if v := strings.TrimSpace(os.Getenv("MINT_PORT")); v != "" {
		port, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("MINT_PORT=%q n'est pas un nombre", v)
		}
		cfg.Port = port
	}
	if v := strings.TrimSpace(os.Getenv("MINT_MAX_BODY_BYTES")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return fmt.Errorf("MINT_MAX_BODY_BYTES=%q invalide", v)
		}
		cfg.MaxBodyBytes = n
	}
	if v := strings.TrimSpace(os.Getenv("MINT_MAX_CAPTURES")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return fmt.Errorf("MINT_MAX_CAPTURES=%q invalide", v)
		}
		cfg.MaxCaptures = n
	}
	if v := strings.TrimSpace(os.Getenv("MINT_MAX_TOTAL_BYTES")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return fmt.Errorf("MINT_MAX_TOTAL_BYTES=%q invalide", v)
		}
		cfg.MaxTotalBodyBytes = n
	}
	if v := strings.TrimSpace(os.Getenv("MINT_MITMDUMP")); v != "" {
		cfg.MitmdumpPath = v
	}
	return cfg.Validate()
}
