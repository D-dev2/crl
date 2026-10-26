#!/bin/sh
# Désinstalle CRL-CLI installé par scripts/install.sh. Le script vérifie
# l'empreinte enregistrée avant de supprimer le binaire.
set -eu

PREFIX="${HOME}/.local"
FORCE=0

usage() {
	cat <<'EOF'
Usage : sh scripts/uninstall.sh [options]

Options :
  --prefix <dir>   préfixe utilisé à l'installation (défaut : ~/.local)
  --force, -f      supprimer le binaire si son empreinte ne correspond plus
  --help, -h       afficher cette aide
EOF
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	--prefix)
		[ "$#" -ge 2 ] || { echo "erreur : --prefix attend un répertoire" >&2; exit 2; }
		PREFIX=$2
		shift 2
		;;
	--prefix=*)
		PREFIX=${1#*=}
		shift
		;;
	--force|-f)
		FORCE=1
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

DEST="$PREFIX/bin/crl"
DATADIR="$PREFIX/share/crl-cli"
MANIFEST="$DATADIR/install-manifest"

hash_file() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	else
		shasum -a 256 "$1" | awk '{print $1}'
	fi
}

if [ ! -e "$DEST" ] && [ ! -L "$DEST" ]; then
	echo "CRL-CLI n'est pas installé à cet emplacement : $DEST"
	rm -f "$MANIFEST"
	rmdir "$DATADIR" 2>/dev/null || true
	exit 0
fi

if [ -L "$DEST" ]; then
	echo "erreur : $DEST est un lien symbolique ; suppression refusée." >&2
	echo "Supprimez-le manuellement si vous êtes certain qu'il vous appartient." >&2
	exit 3
fi

OWNED=0
if [ -f "$MANIFEST" ]; then
	IFS=$(printf '\t') read -r OLD_HASH OLD_PATH <"$MANIFEST" || true
	if [ "${OLD_PATH:-}" = "$DEST" ] && [ "${OLD_HASH:-}" = "$(hash_file "$DEST")" ]; then
		OWNED=1
	fi
fi

if [ "$OWNED" -ne 1 ] && [ "$FORCE" -ne 1 ]; then
	echo "erreur : impossible de vérifier que $DEST a été installé par ce script." >&2
	echo "Aucun fichier n'a été supprimé. Utilisez --force après vérification." >&2
	exit 3
fi

rm -f "$DEST"
rm -f "$MANIFEST"
rmdir "$DATADIR" 2>/dev/null || true
rmdir "$PREFIX/bin" 2>/dev/null || true
echo "CRL-CLI désinstallé : $DEST"
if command -v crl >/dev/null 2>&1; then
	echo "Attention : une autre commande crl reste accessible dans le PATH : $(command -v crl)" >&2
fi
