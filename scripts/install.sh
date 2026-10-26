#!/bin/sh
# Installe CRL-CLI dans <préfixe>/bin sans écraser silencieusement un fichier
# personnel. Le préfixe par défaut est ~/.local.
set -eu

PREFIX="${HOME}/.local"
FORCE=0

usage() {
	cat <<'EOF'
Usage : sh scripts/install.sh [options]

Options :
  --prefix <dir>   préfixe d'installation (défaut : ~/.local)
  --force, -f      remplacer un fichier crl non reconnu après sauvegarde
  --help, -h       afficher cette aide

Le binaire est compilé depuis le dépôt courant, puis installé dans
<préfixe>/bin/crl. Un manifeste contenant son empreinte SHA-256 est conservé
dans <préfixe>/share/crl-cli/install-manifest.
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

[ -n "$PREFIX" ] || { echo "erreur : préfixe vide" >&2; exit 2; }

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
BINDIR="$PREFIX/bin"
DEST="$BINDIR/crl"
DATADIR="$PREFIX/share/crl-cli"
MANIFEST="$DATADIR/install-manifest"
GO_BIN=${GO:-go}

need() {
	if ! command -v "$1" >/dev/null 2>&1; then
		echo "erreur : $1 est requis mais introuvable dans PATH" >&2
		exit 1
	fi
}

hash_file() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	else
		shasum -a 256 "$1" | awk '{print $1}'
	fi
}

need "$GO_BIN"
need curl
if ! command -v mitmdump >/dev/null 2>&1; then
	echo "avertissement : mitmdump est absent ; Mint restera désactivé jusqu'à l'installation de mitmproxy." >&2
fi

# Évite une compilation obscure si le binaire Go et son GOROOT proviennent de
# versions différentes (par exemple /usr/local/go/bin/go avec un GOROOT asdf).
GO_VERSION=$("$GO_BIN" version | awk '{print $3}')
GO_ROOT=$("$GO_BIN" env GOROOT)
if [ -r "$GO_ROOT/VERSION" ]; then
	ROOT_VERSION=$(sed -n '1p' "$GO_ROOT/VERSION")
	if [ "$GO_VERSION" != "$ROOT_VERSION" ]; then
		cat >&2 <<EOF
erreur : installation Go incohérente.
Le compilateur sélectionné est $GO_VERSION, mais $GO_ROOT contient $ROOT_VERSION.
Sélectionnez le binaire Go et le GOROOT de la même installation, puis relancez.
Vous pouvez choisir le binaire avec : GO=/chemin/vers/go sh scripts/install.sh
EOF
		exit 1
	fi
fi

mkdir -p "$BINDIR" "$DATADIR"
TMP_BUILD=$(mktemp "${TMPDIR:-/tmp}/crl-build.XXXXXX")
TMP_INSTALL=$(mktemp "$BINDIR/.crl-install.XXXXXX")
cleanup() {
	rm -f "$TMP_BUILD" "$TMP_INSTALL"
}
trap cleanup EXIT HUP INT TERM

echo "Compilation de CRL-CLI..."
(cd "$REPO_ROOT" && "$GO_BIN" build -trimpath -o "$TMP_BUILD" ./cmd/crl)
chmod 0755 "$TMP_BUILD"
NEW_HASH=$(hash_file "$TMP_BUILD")

OWNED=0
if [ -f "$MANIFEST" ] && [ -f "$DEST" ] && [ ! -L "$DEST" ]; then
	IFS=$(printf '\t') read -r OLD_HASH OLD_PATH <"$MANIFEST" || true
	if [ "${OLD_PATH:-}" = "$DEST" ] && [ "${OLD_HASH:-}" = "$(hash_file "$DEST")" ]; then
		OWNED=1
	fi
fi

if [ -L "$DEST" ]; then
	echo "erreur : $DEST est un lien symbolique ; installation refusée pour ne pas le remplacer." >&2
	echo "Supprimez ou déplacez ce lien manuellement, puis relancez." >&2
	exit 3
fi
if [ -e "$DEST" ] && [ "$OWNED" -ne 1 ]; then
	if [ "$FORCE" -ne 1 ]; then
		echo "erreur : $DEST existe déjà et n'est pas reconnu comme installé par ce script." >&2
		echo "Aucun fichier n'a été modifié. Utilisez --prefix ou, après vérification, --force." >&2
		exit 3
	fi
	BACKUP="$DEST.bak.$(date +%Y%m%d%H%M%S)"
	cp -p "$DEST" "$BACKUP"
	echo "Sauvegarde du fichier existant : $BACKUP"
fi

# On prépare le binaire à côté de sa destination pour que le remplacement soit
# aussi atomique que possible sur le même système de fichiers.
cp "$TMP_BUILD" "$TMP_INSTALL"
chmod 0755 "$TMP_INSTALL"
mv -f "$TMP_INSTALL" "$DEST"

printf '%s\t%s\n' "$NEW_HASH" "$DEST" >"$MANIFEST"

echo "CRL-CLI installé : $DEST"
echo "Version : $("$DEST" --version)"
echo "Lancement : crl"
if command -v crl >/dev/null 2>&1; then
	echo "La commande crl est déjà accessible dans le PATH courant."
else
	echo "Ajoutez $BINDIR au PATH de votre shell si nécessaire :"
	echo "  export PATH=\"$BINDIR:\$PATH\""
fi
if command -v fish >/dev/null 2>&1; then
	echo "Pour Fish, configurez le PATH avec : sh scripts/install-fish-path.sh"
fi
echo "Désinstallation : sh scripts/uninstall.sh --prefix \"$PREFIX\""
