#!/bin/sh
# Configure le PATH de Fish pour que la commande crl soit disponible.
# Le script utilise fish_add_path et ne modifie pas le fichier config.fish.
set -eu

BINDIR="${HOME}/.local/bin"

usage() {
	cat <<'EOF'
Usage : sh scripts/install-fish-path.sh [--prefix <dir>]

Ajoute <préfixe>/bin (défaut : ~/.local/bin) au PATH universel de Fish.
Si Fish n'est pas installé, le script affiche les instructions et ne modifie
rien.
EOF
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	--prefix)
		[ "$#" -ge 2 ] || { echo "erreur : --prefix attend un répertoire" >&2; exit 2; }
		BINDIR="$2/bin"
		shift 2
		;;
	--prefix=*)
		BINDIR="${1#*=}/bin"
		shift
		;;
	--help|-h)
		usage
		exit 0
		;;
	*)
		echo "option inconnue : $1" >&2
		usage >&2
		exit 2
		;;
	esac
done

if ! command -v fish >/dev/null 2>&1; then
	echo "Fish n'est pas installé : aucune modification effectuée." >&2
	echo "Installez Fish, puis relancez : sh scripts/install-fish-path.sh" >&2
	exit 0
fi

if fish -c 'contains -- $argv[1] $PATH' "$BINDIR" >/dev/null 2>&1; then
	echo "$BINDIR est déjà présent dans le PATH de Fish."
	exit 0
fi

# fish_add_path évite les doublons et stocke le chemin dans les variables
# universelles de Fish, sans éditer un fichier personnel.
fish -c 'fish_add_path -U -p $argv[1]' "$BINDIR"
echo "PATH Fish configuré : $BINDIR"
echo "Ouvrez un nouveau terminal Fish, puis vérifiez : command -v crl"
