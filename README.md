# CRL-CLI

CRL-CLI est un shell interactif en Go pour construire, inspecter et rejouer des
requêtes HTTP avec `curl`. Il intègre **Mint**, un gestionnaire de proxy
[mitmproxy](https://mitmproxy.org/) qui capture le trafic HTTP(S), conserve un
historique en mémoire et permet d'importer une requête capturée dans le
constructeur `crl.*` sans l'envoyer.

- Constructeur de requêtes : URL, méthode, en-têtes, corps JSON ou brut.
- Rejeu fidèle via `curl`, sans passer les données par un shell.
- Réponses HTTP présentées avec statut, durée, en-têtes et corps lisible.
- Modes d'affichage `auto`, `raw` et `pretty`, sans modifier les octets reçus.
- Prompt compact sur une ligne : `crl [GET] [Mint: ON] [Sel: N] >`, avec bilan du dernier rejeu multiple.
- Édition JSON multiligne intégrée au shell (`:end` pour valider, `:cancel` pour annuler), sans `$EDITOR`/`$VISUAL`. Modification ciblée avec `crl.json set <chemin> = <valeur JSON>` ou suppression avec `crl.json delete <chemin>`.
- Sélection de plusieurs captures Mint et rejeu concurrent avec `mint.replay`.
- Proxy Mint démarré et arrêté par CRL-CLI, avec confirmation de disponibilité.
- Capture en temps réel, historique, filtre, sélection et import.
- Affichage complet dans les commandes d'inspection, URL masquée dans les tableaux et notifications Mint.
- Aucune dépendance Go externe : bibliothèque standard uniquement.
- Installation portable dans `~/.local/bin`, sans chemin personnel en dur.

> CRL-CLI ne capture pas « tout le trafic de la machine ». Mint ne voit que le
> trafic qui passe réellement par son proxy. Il n'installe pas de certificat CA
> et ne fait pas d'interception transparente.

## Sommaire

- [Prérequis](#prérequis)
- [Compilation et lancement](#compilation-et-lancement)
- [Installation et désinstallation](#installation-et-désinstallation)
- [Démarrage rapide](#démarrage-rapide)
- [Commandes CRL](#commandes-crl)
- [Commandes Mint](#commandes-mint)
- [Proxy automatique et priorité](#proxy-automatique-et-priorité)
- [Réponses HTTP et modes d'affichage](#réponses-http-et-modes-daffichage)
- [HTTPS et certificat CA](#https-et-certificat-ca)
- [Configuration](#configuration)
- [Affichage, couleurs et lisibilité](#affichage-couleurs-et-lisibilité)
- [Sécurité et architecture](#sécurité-et-architecture)
- [Limites et comportements explicites](#limites-et-comportements-explicites)
- [Tests](#tests)
- [Structure du projet](#structure-du-projet)
- [Dépannage](#dépannage)
- [Licence](#licence)

## Prérequis

| Outil | Rôle | Remarque |
|---|---|---|
| Go 1.22 ou plus récent | compilation | aucune dépendance Go externe |
| `curl` | envoi des requêtes | vérifié au moment de `crl.launch` |
| `mitmdump` (mitmproxy) | capture Mint | uniquement nécessaire pour Mint |
| Linux ou macOS | édition interactive | ailleurs, le shell fonctionne en mode non interactif |

Installation de mitmproxy, au choix :

```sh
pipx install mitmproxy
# ou
python3 -m pip install --user mitmproxy
```

Vérifiez que `mitmdump` est dans le `PATH` :

```sh
mitmdump --version
```

Si `mitmdump` est installé ailleurs, utilisez `MINT_MITMDUMP=/chemin/vers/mitmdump`
ou laissez CRL-CLI afficher un message explicite au démarrage.

## Compilation et lancement

Depuis la racine du dépôt :

```sh
go build -trimpath -o crl ./cmd/crl
./crl
```

Ou directement :

```sh
go run ./cmd/crl
```

Options disponibles :

```sh
./crl --version
./crl --help
```

Au démarrage, CRL-CLI affiche un avertissement si `mitmdump` n'est pas détecté.
Le shell reste utilisable sans Mint. Le binaire est autonome : l'addon Python de
Mint et les styles sont embarqués, donc il peut être lancé depuis n'importe quel
répertoire.

## Installation et désinstallation

### Installation locale

Le script compile le projet et installe la commande dans `~/.local/bin` :

```sh
sh scripts/install.sh
```

Pour choisir un autre préfixe :

```sh
sh scripts/install.sh --prefix "$HOME/.local"
```

Le script :

- vérifie Go et curl ;
- signale l'absence de `mitmdump` sans bloquer l'installation ;
- compile avec `-trimpath` ;
- refuse d'écraser silencieusement un fichier `crl` non reconnu ;
- en cas de conflit et avec `--force`, sauvegarde le fichier existant avant
  remplacement ;
- enregistre l'empreinte SHA-256 du binaire dans
  `<préfixe>/share/crl-cli/install-manifest`.

Après installation, la commande exacte est :

```sh
crl
```

Pour **zsh**, vérifiez le `PATH` dans un nouveau terminal :

```sh
zsh -lic 'command -v crl'
```

Si la commande n'est pas trouvée, ajoutez ceci à `~/.zshrc` :

```sh
export PATH="$HOME/.local/bin:$PATH"
```

Puis rouvrez le terminal ou exécutez `source ~/.zshrc`. Pour **Fish**
(facultatif), un script dédié configure le chemin universel sans modifier
`config.fish` :

```sh
sh scripts/install-fish-path.sh
```

Puis, dans un nouveau terminal Fish :

```fish
command -v crl
crl
```

Le script Fish utilise `fish_add_path`. Si Fish n'est pas installé, il ne modifie
rien et affiche les instructions.

### Désinstallation

```sh
sh scripts/uninstall.sh
```

La désinstallation vérifie le manifeste et l'empreinte du binaire avant de le
supprimer. En cas de doute, elle refuse et demande `--force`. Elle ne supprime
pas le répertoire `bin` s'il contient d'autres fichiers.

### Depuis un autre ordinateur Linux

```sh
git clone <dépôt> crl-cli
cd crl-cli
go test ./...
sh scripts/install.sh
crl
```

Aucun chemin absolu personnel n'est présent dans le code ou les scripts. Les
chemins affichés à l'exécution (`mitmdump`, CA, fichiers temporaires) proviennent
de l'environnement de l'utilisateur.

## Démarrage rapide

### Construire et envoyer une requête avec curl

```text
crl.host http://127.0.0.1:5002/users/v1/login?next=%2Fhome
crl.method POST
crl.head Content-Type: application/json
crl.head Authorization: Bearer mon-jeton
crl.json {"username":"demo","password":"secret"}
crl.show
crl.table
crl.launch
```

Sans argument, `crl.json` entre en mode saisie JSON multiligne intégrée au shell
(affiche le corps actuel en référence, saisie avec `:end` pour valider ou `:cancel` pour annuler).
`crl.body` sans argument propose une saisie multiligne intégrée similaire sans validation JSON.
`crl.json set <chemin> = <valeur JSON>` et `crl.json set user.role = "admin"` modifient un champ
précis en préservant les autres propriétés. `crl.json delete <chemin>` supprime une propriété.
`crl.json clear` et `crl.body.clear` effacent le corps sans toucher aux autres en-têtes.

Pour rejouer simultanément plusieurs captures :

```text
mint.select 12 15
mint.selected
mint.replay
```

La sélection reste active jusqu'à `mint.select clear` ou une nouvelle
sélection. Le rejeu démarre jusqu'à quatre requêtes à la fois et affiche un
bilan par identifiant après la fin du lot. Le rejeu contourne le proxy Mint
local afin d'éviter de générer une nouvelle capture artificielle. `mint.use` sans ID demande une
sélection unique ; avec plusieurs captures, utilisez explicitement
`mint.use <id>`.

À l'affichage, `crl.launch` présente d'abord la requête, puis le statut, la durée,
les en-têtes de réponse et le corps. Un JSON valide est indenté ; une page HTML
est aérée sans être présentée comme du JSON. Pour conserver le corps brut :

```text
crl.output raw
crl.launch
```

`crl.output auto` rétablit la détection automatique.

`crl.show` et `mint.request.show` affichent l'URL complète (paramètres compris)
et les valeurs complètes des en-têtes de requête ; `mint.request.show` affiche
aussi les en-têtes de réponse. `crl.launch` affiche l'URL complète et les
en-têtes de réponse complets. Ces sorties peuvent contenir des identifiants,
des jetons ou des cookies : attention avant de les partager. `crl.table`, les
tableaux Mint et les notifications Mint masquent les valeurs sensibles de
l'URL et n'affichent pas les valeurs d'en-têtes.

### Capturer une requête avec Mint et l'importer

```text
mint.proxy on
crl.host http://127.0.0.1:5002/users/v1/login
crl.method POST
crl.json {"username":"demo"}
crl.launch

mint.requests
mint.request.show 1
mint.select 1
mint.use 1
crl.show
crl.launch
mint.proxy off
```

Quand Mint est actif et qu'aucun proxy explicite n'est configuré, `crl.launch`
utilise automatiquement le proxy Mint. L'historique reste disponible après
`mint.proxy off` jusqu'à la fin de la session CRL-CLI.

## Commandes CRL

| Commande | Description |
|---|---|
| `crl.host <url>` | définit l'URL ; sans schéma, `http://` est ajouté ; l'encodage, les paramètres répétés et les valeurs vides sont conservés |
| `crl.host` | affiche l'URL courante (masquée) |
| `crl.method <méthode>` | définit la méthode du constructeur actif (normalisée en majuscules) |
| `crl.method <méthode> set <cible>` | modifie la méthode des captures Mint ciblées (`1`, `1,2`, `selected`, `all`) |
| `crl.head <Nom: valeur>` | ajoute ou remplace un en-tête (alias : `crl.header`) |
| `crl.head` | liste les en-têtes avec leurs valeurs complètes |
| `crl.head clear [Nom]` | supprime un en-tête nommé, ou tous sans argument |
| `crl.head set <cible> <Nom> <valeur>` | ajoute/remplace l'en-tête des captures ciblées ; les valeurs entre guillemets gardent leurs espaces |
| `crl.head delete <cible> <Nom>` | supprime l'en-tête des captures ciblées |
| `crl.head show <cible>` | affiche les en-têtes masquant les valeurs sensibles |
| `crl.head.clear [Nom]` | variante explicite de la commande précédente |
| `crl.json <json>` | définit un corps JSON valide et `Content-Type: application/json` |
| `crl.json` | saisie multiligne intégrée (`:end` pour valider, `:cancel` pour annuler) |
| `crl.json set <chemin> = <valeur JSON>` | modifie un champ précis (objets imbriqués pris en charge) |
| `crl.json delete <chemin>` | supprime une propriété précise |
| `crl.json <cible> set <chemin> = <valeur JSON>` | applique le changement JSON aux captures ciblées sans toucher les originales |
| `crl.json <id> edit` | édite le JSON de cette capture seulement ; `:end` applique après validation, `:cancel` annule |
| `crl.json clear` | efface le corps sans modifier méthode ni en-têtes |
| `crl.body <texte>` | définit un corps brut, sans modifier le `Content-Type` |
| `crl.body` | saisie multiligne intégrée sans validation JSON (`:end`/`:cancel`) |
| `crl.body <id> edit` | édite le corps brut d'une capture seulement |
| `crl.body <cible> set <texte>` / `clear` | définit ou efface les corps des captures ciblées |
| `crl.body.clear` | supprime le corps |
| `crl.proxy <url\|none\|auto>` | proxy explicite ; `none` désactive tout proxy ; `auto`, `clear` ou `unset` réactive le proxy Mint automatique |
| `crl.proxy` | affiche le proxy explicite et le proxy effectif |
| `crl.ca <fichier\|off>` | utilise un fichier CA PEM pour curl, sans désactiver la vérification TLS ; `off`/`none`/`clear` revient au bundle système |
| `crl.show` | détaille la requête active ou toutes les captures sélectionnées ; secrets masqués, corps volumineux paginés |
| `crl.show <id> [body <offset>]` | détaille une capture et lit une page de son corps |
| `crl.show active` | détaille le constructeur actif même lorsqu'une sélection Mint existe |
| `crl.show body [offset]` | lit une page du corps actif |
| `crl.table` | affiche un tableau récapitulatif (URL masquée, noms d'en-têtes ; valeurs via `crl.show`) |
| `crl.output [auto\|raw\|pretty]` | choisit le format d'affichage du corps des réponses ; sans argument, affiche le mode courant |
| `crl.launch` | envoie la sélection si elle existe, sinon le constructeur actif |
| `crl.launch active` | envoie explicitement le constructeur actif |
| `crl.launch <cible>` | envoie en parallèle des captures Mint ciblées (`1`, `1,2`, `selected`, `all`) |
| `reset` | réinitialise le constructeur |
| `clear` | efface l'écran en mode interactif |
| `help` ou `?` | affiche l'aide |
| `exit` ou `quit` | quitte et arrête proprement le proxy Mint lancé par CRL-CLI |

Notes :

- `crl.launch` utilise `curl --globoff --path-as-is` et passe les en-têtes et le
  corps directement en arguments/stdin, sans invocation de shell.
- Les en-têtes recalculés ou interdits par curl (`Content-Length`,
  `Transfer-Encoding`, `Connection`, `Proxy-Connection`, `Expect`) ne sont pas
  rejoués.
- `crl.launch` n'accepte aucun argument.
- Le corps est transmis à curl via stdin avec `--data-binary @-`.
- Les en-têtes et le corps de réponse sont écrits dans un répertoire temporaire
  privé, puis lus pour l'affichage. Le formatage (JSON/HTML) ne s'applique qu'à
  l'affichage : les octets bruts restent disponibles dans le fichier indiqué.
- Un statut HTTP 4xx ou 5xx est une réponse réelle du serveur, pas une erreur de
  connexion. Les délais dépassés et les erreurs réseau sont présentés séparément.
- Les réponses 400, 401, 403 et 404 affichent une indication de diagnostic.
  Quand `crl.launch` utilise le proxy Mint, CRL attend brièvement une capture
  récente de même méthode et URL, puis signale les différences d'en-têtes
  observées. En cas de trafic concurrent, de capture tardive ou absente, la
  corrélation n'est pas certaine ; comparez `crl.show` avec `mint.requests` et
  `mint.request.show <id>` manuellement.
- Un statut 403 ne signifie pas nécessairement qu'un en-tête d'authentification
  manque : le serveur peut refuser la requête pour une autre règle d'accès.
- L'affichage du corps est limité à 32 Kio ; toute troncature est signalée et le
  chemin du corps complet est affiché. Le fichier temporaire est supprimé au
  prochain `crl.launch` ou à la fermeture du shell.

## Commandes Mint

| Commande | Description |
|---|---|
| `mint.proxy on` | démarre `mitmdump` et attend que l'addon confirme que le proxy est prêt |
| `mint.proxy off` | arrête uniquement le processus lancé par CRL-CLI ; les captures restent en mémoire |
| `mint.proxy status` | état, adresse d'écoute, nombre de requêtes/réponses, état du pont, dernière erreur |
| `mint.proxy config` | configuration effective et prérequis |
| `mint.proxy.port <port>` | change le port d'écoute ; refusé si le proxy n'est pas arrêté |
| `mint.requests` | liste les captures, filtre appliqué |
| `mint.watch on\|off` | active ou suspend les notifications automatiques ; les captures restent enregistrées |
| `mint.events` | affiche les notifications mises en attente pendant `mint.watch off` |
| `mint.history` | liste tout l'historique, sans filtre |
| `mint.request.show <id>` | détail complet d'une capture (alias : `mint.show`) |
| `mint.select <id...>` | remplace la sélection ; `add <cible>`, `remove <cible>`, `clear`, `all` la modifient |
| `mint.selected` | affiche la sélection persistante |
| `mint.replay [cible]` | rejoue en parallèle les captures sélectionnées ou ciblées (maximum 4 simultanées) |
| `mint.replay.body <id> [offset]` | consulte les pages du corps de réponse (jusqu'à 16 Mio, durant la session) |
| `mint.filter <texte>` | filtre localement par hôte, URL, chemin ou chaîne de requête |
| `mint.filter clear` | supprime le filtre sans effacer l'historique |
| `mint.clear [--yes]` | efface l'historique après confirmation ; `--yes` ou `-y` évite la question |
| `mint.use [id] [--force]` | importe une capture dans le constructeur CRL-CLI, sans envoyer de requête |
| `mint.help` | affiche l'aide Mint |

Avec `mint.watch off`, les notifications sont mises en attente sans interrompre
la saisie. La file est plafonnée à 1024 événements ; au-delà, les plus anciens
sont abandonnés et `mint.events` en indique le nombre.

Les événements Mint sont lus et affichés par la goroutine du shell : le
gestionnaire du proxy et le pont ne touchent jamais directement au terminal.
Pendant la saisie, l'éditeur repart de l'origine mémorisée du prompt, efface
tout le bloc affiché (y compris les lignes de continuation d'un collage long),
affiche les notifications au-dessus, puis restaure le prompt et le texte déjà
tapé. L'éditeur reste celui de la bibliothèque standard, sans dépendance
externe de type readline.

Comportement de `mint.use` :

- sans `id`, utilise la capture sélectionnée par `mint.select` ;
- refuse l'import si le corps de requête est tronqué ;
- accepte une réponse encore en attente, avec un avertissement ;
- accepte une réponse dont le corps est tronqué, avec un avertissement ; ce
  corps n'est pas utilisé pour le rejeu ;
- demande confirmation avant de remplacer une configuration non vide, sauf avec
  `--force` ;
- marque cette capture comme requête active du constructeur sans modifier la sélection Mint ;
- n'envoie jamais la requête.

## Proxy automatique et priorité

Ordre de priorité pour `crl.launch` :

1. proxy explicite configuré avec `crl.proxy <url>` ;
2. proxy Mint automatique si `mint.proxy on` est actif ;
3. comportement par défaut de curl, qui peut utiliser les variables
   d'environnement `http_proxy`, `https_proxy`, etc.

Cas particuliers :

- `crl.proxy none` ou `crl.proxy off` force l'absence de proxy, même si Mint est
  actif ;
- `crl.proxy auto` (ou `clear`/`unset`) supprime le proxy explicite et rend le
  proxy Mint automatique de nouveau applicable ;
- quand le proxy Mint est utilisé, CRL-CLI ajoute `--noproxy ""` afin que le
  trafic loopback passe réellement par le proxy. Sans cette option, curl
  contournerait souvent `127.0.0.1` et `localhost` ;
- un proxy explicite reste prioritaire sur Mint, y compris pour le loopback.

Pour un client curl lancé hors CRL-CLI, utilisez par exemple :

```sh
curl --proxy http://127.0.0.1:8080 --noproxy "" http://127.0.0.1:5002/
```

`--noproxy ""` est important pour capturer les requêtes vers la machine locale.

## Réponses HTTP et modes d'affichage

`crl.launch` sépare l'affichage en sections :

1. résumé de la requête envoyée (méthode, URL complète, proxy, mode) ;
2. statut HTTP, durée, taille, type et version ;
3. en-têtes de réponse ;
4. corps de la réponse.

Les statuts utilisent la palette suivante : 2xx en vert, 3xx en cyan, 4xx en
jaune et 5xx en rouge. Un statut HTTP n'est jamais remplacé par un message
générique : une page d'erreur 400 est affichée avec son statut, ses en-têtes et
son HTML.

Trois modes sont disponibles :

| Mode | Comportement |
|---|---|
| `auto` (défaut) | JSON valide indenté, HTML aéré, autre contenu brut |
| `raw` | corps affiché sans indentation ni mise en forme |
| `pretty` | JSON indenté et HTML aéré, sans transformer le reste |

```text
crl.output auto
crl.output raw
crl.output pretty
crl.output
```

Le formatage ne modifie jamais les données reçues ni les octets rejoués. Le mode
`raw` n'active pas les séquences ANSI du serveur : les caractères de contrôle
sont neutralisés pour protéger le terminal.

Pour les erreurs de transport, CRL-CLI distingue :

- **délai dépassé** (curl 28) ;
- **erreur de connexion** (DNS, refus de connexion, TLS, réponse vide) ;
- **autre échec curl** (écriture locale, interruption, etc.).

Si aucun statut HTTP n'a été reçu, aucun statut n'est inventé. Si une réponse
partielle existe malgré l'erreur, elle est affichée et signalée.

Le corps complet est écrit dans un fichier temporaire privé et son chemin est
affiché. Il peut être consulté avec `cat` :

```sh
cat /tmp/crl-response-.../body.bin
cat /tmp/crl-response-.../headers.txt
```

Le répertoire est supprimé au prochain `crl.launch` ou à la fermeture de
CRL-CLI. L'affichage direct est limité à 32 Kio avec un avertissement explicite ;
le fichier contient la réponse complète.

## HTTPS et certificat CA

mitmproxy intercepte HTTPS en présentant un certificat signé par son propre CA.
curl doit faire confiance à ce CA, sinon la connexion échoue. CRL-CLI **ne
désactive jamais** la vérification TLS et **n'installe pas** le CA
automatiquement.

Après un premier démarrage de Mint, le certificat est généralement disponible
dans `~/.mitmproxy/mitmproxy-ca-cert.pem`. Indiquez-le à CRL-CLI :

```text
mint.proxy on
mint.proxy off
crl.ca ~/.mitmproxy/mitmproxy-ca-cert.pem
crl.host https://example.test/
crl.launch
```

`crl.ca` vérifie que le fichier existe et passe `--cacert <fichier>` à curl. La
vérification TLS reste active. Pour d'autres clients, configurez leur magasin de
confiance ou leur variable de CA selon leur documentation.

## Configuration

Variables d'environnement lues au démarrage :

| Variable | Défaut | Rôle |
|---|---:|---|
| `MINT_PORT` | `8080` | port d'écoute initial du proxy (loopback uniquement) |
| `MINT_MAX_BODY_BYTES` | `2097152` (2 Mio) | taille maximale conservée pour un corps de requête ou de réponse |
| `MINT_MAX_CAPTURES` | `2000` | nombre maximal de captures en mémoire |
| `MINT_MAX_TOTAL_BYTES` | `67108864` (64 Mio) | somme maximale des corps conservés |
| `MINT_MITMDUMP` | recherche dans `PATH` | chemin explicite de `mitmdump` |

Exemples :

```sh
MINT_PORT=8081 ./crl
MINT_MAX_CAPTURES=500 MINT_MAX_TOTAL_BYTES=33554432 ./crl
MINT_MITMDUMP="$HOME/.local/bin/mitmdump" ./crl
```

Une valeur invalide provoque un arrêt immédiat avec un message explicite.
`mint.proxy.port <port>` modifie le port pour la session courante ; la variable
`MINT_PORT` ne s'applique qu'au démarrage.

## Affichage, couleurs et lisibilité

Les couleurs sont activées automatiquement quand la sortie standard est un
terminal. Elles restent désactivées pour les redirections, les pipes et les
tests, afin de ne jamais polluer une sortie machine.

- l'invite `crl >` est cyan après une commande réussie et rouge après une
  erreur ;
- les méthodes HTTP, les statuts et les identifiants de capture utilisent des
  couleurs distinctes dans `mint.requests` et `mint.request.show` ;
- les titres de section et les libellés sont également différenciés ;
- `crl.launch` colore les statuts 2xx, 3xx, 4xx et 5xx et sépare la requête, les
  en-têtes et le corps ;
- `mint.proxy.config` affiche les états `ACTIF`, `ARRÊTÉ`, `ERREUR` et
  `EN ATTENTE`, le processus, l'addon, le pont, les captures et les deux niveaux
  de proxy (CRL et Mint) sans les confondre ;
- `crl.show`, `crl.table` et `mint.request.show` utilisent des champs alignés
  et des séparateurs pour rendre les données lisibles ;
- les corps JSON valides sont indentés automatiquement à l'affichage, sans
  modifier les octets réellement conservés ou rejoués ;
- les pages HTML sont aérées pour la lecture mais identifiées comme HTML, jamais
  comme JSON.

Pour désactiver les couleurs :

```sh
NO_COLOR=1 ./crl
CLICOLOR=0 ./crl
```

Un terminal `TERM=dumb` désactive aussi les styles.

## Sécurité et architecture

CRL-CLI est un processus Go unique. L'addon mitmproxy est écrit en Python, mais
il est embarqué dans le binaire Go et extrait dans un répertoire temporaire au
démarrage du proxy, puis supprimé à l'arrêt.

- **Écoute loopback uniquement** : le proxy et le pont local écoutent sur
  `127.0.0.1`. Une adresse non loopback est refusée.
- **Jeton de session** : le pont reçoit un jeton aléatoire de 32 octets, encodé
  en hexadécimal. Chaque événement doit fournir
  `Authorization: Bearer <jeton>`. La comparaison est à temps constant.
- **Jeton non journalisé** : le jeton est transmis à l'addon par variable
  d'environnement du processus enfant et n'apparaît pas dans les messages
  normaux. Les éventuelles traces d'erreur de mitmproxy sont expurgées.
- **Pas de proxy système pour le pont** : l'addon contacte le pont en direct avec
  `http.client`, sans utiliser les variables de proxy de l'environnement, ce qui
  évite les boucles de capture.
- **Limites de taille** : nombre d'en-têtes, taille des en-têtes, URL, corps,
  durée et taille globale des événements sont validés avant stockage.
- **Mémoire seulement pour Mint** : aucun fichier d'historique, aucune
  télémétrie, aucun envoi réseau autre que le pont local et les requêtes que vous
  demandez. Les réponses de `crl.launch` passent par un répertoire temporaire
  privé, supprimé au prochain envoi ou à la fermeture.
- **Fichiers temporaires de réponse** : répertoire créé avec les droits `0700`,
  fichiers `headers.txt` et `body.bin` en `0600`. Le corps brut n'est jamais
  réécrit ni renvoyé automatiquement.
- **Installation vérifiable** : le script d'installation enregistre l'empreinte
  SHA-256 du binaire. La désinstallation refuse de supprimer un fichier dont
  l'empreinte ne correspond plus, sauf `--force` explicite.
- **Cycle de vie maîtrisé** : `mint.proxy off` et `exit` arrêtent uniquement le
  groupe de processus lancé par CRL-CLI. Aucun processus mitmproxy arbitraire
  n'est tué.
- **Disponibilité vérifiée** : `mint.proxy on` ne se déclare réussi qu'après un
  événement `ready` envoyé par l'addon. En cas d'échec, de délai dépassé ou de
  crash, le shell reste utilisable et signale l'erreur.
- **Captures conservées** : arrêter le proxy ne supprime pas l'historique de la
  session. Quitter CRL-CLI le supprime, car tout est en mémoire.
- **Affichage des secrets** : les inspections détaillées masquent les valeurs
  sensibles des en-têtes, les champs JSON usuels (jetons, mots de passe) et les
  paramètres d'URL connus. Les données d'import et de rejeu ne sont jamais
  modifiées.

### Association et troncature

Les requêtes et réponses sont associées par l'identifiant de flux mitmproxy
(`flow_id`), jamais par leur position dans une liste. Une réponse peut arriver
avant l'enregistrement complet de la requête ou après l'arrêt du proxy : la
capture est alors marquée incomplète ou en attente.

- Une capture dont le corps de requête est tronqué ne peut pas être importée.
- Une réponse absente ou dont le corps est tronqué peut être importée avec un
  avertissement explicite.
- La troncature est affichée avec la taille annoncée et la taille conservée.
- Les limites mémoire peuvent évincer les captures les plus anciennes. L'ID
  d'une capture n'est jamais réutilisé pendant la session.

## Limites et comportements explicites

- **Pas de capture globale** : seul le trafic configuré pour utiliser le proxy
  Mint est capturé.
- **Pas de persistance Mint** : l'historique est perdu à la fermeture de
  CRL-CLI. Il n'y a ni export, ni rechargement, ni base de données.
- **Réponse brute temporaire** : le corps complet d'un `crl.launch` est
  consultable dans le fichier temporaire indiqué, mais il disparaît au prochain
  envoi ou à la fermeture. Pour le conserver, copiez-le explicitement avant de
  quitter.
- **Affichage de réponse borné** : seuls les 32 premiers Kio sont affichés, avec
  un message de troncature. Le fichier temporaire reste complet.
- **Pas d'installation automatique du CA** : HTTPS nécessite une configuration
  de confiance côté client.
- **Pas d'interception transparente** : Mint ne modifie pas les règles réseau du
  système.
- **Données sensibles affichées** : les commandes d'inspection affichent les
  URL complètes ainsi que les valeurs d'en-têtes et les corps tels quels.
  Protégez leurs sorties si elles contiennent des secrets. Les URL dans les
  tableaux récapitulatifs restent formatées pour limiter le bruit.
- **Troncature** : les corps dépassant `MINT_MAX_BODY_BYTES` sont tronqués et
  signalés. Un corps de requête tronqué bloque l'import.
- **Filtre local** : `mint.filter` ne fait qu'un filtrage de texte insensible à
  la casse sur l'hôte, l'URL, le chemin et la query string. Il ne modifie ni le
  proxy ni l'historique.
- **Éditeur interactif** : l'édition de ligne brute est prise en charge sous
  Linux et macOS. Sur les autres plateformes, ou si l'entrée n'est pas un
  terminal, le shell bascule en mode ligne par ligne.
- **curl obligatoire pour l'envoi** : CRL-CLI prépare la requête mais délègue
  l'exécution à `curl`.

## Tests

Commandes de validation exécutées sur le projet :

```sh
gofmt -w .
go test ./...
go vet ./...
go test -race ./...
go build ./cmd/crl
```

Résultats constatés lors de la dernière validation :

- `gofmt -w .` : aucun écart restant ;
- `go test ./...` : réussi ;
- `go vet ./...` : réussi ;
- `go test -race ./...` : réussi ;
- `go build ./cmd/crl` : réussi.

Les tests couvrent notamment :

- affichage complet des en-têtes et URL dans les commandes d'inspection,
  masquage dans les tableaux et notifications Mint ;
- construction d'arguments curl, priorité des proxies, import et refus de
  capture incomplète ;
- historique : association par `flow_id`, réponses en attente, erreurs,
  mises à jour, filtre, sélection, limites mémoire et éviction ;
- pont local : authentification, JSON invalide, taille excessive, événements
  séquentiels ;
- gestionnaire Mint : démarrage/arrêt, crash, délai de démarrage, port occupé,
  redémarrage, arrêt propre et non-agression d'un processus sans rapport ;
- shell : commandes, détails multisélectionnés, modifications individuelles et
  collectives, import, confirmation, lancement curl,
  rejeu des en-têtes `Authorization`/`Cookie` après sélection et import,
  diagnostics 400/401/403/404 et utilisabilité après un échec de démarrage de
  Mint ;
- notifications Mint : file bornée avec `mint.watch off`, corrélation
  asynchrone avec une capture récente et test PTY d'une notification reçue
  pendant qu'une commande est en cours de saisie ;
- réponses : analyse des en-têtes finaux, métadonnées curl, JSON/HTML, modes
  `auto`, `raw`, `pretty`, troncature, corps binaires et distinction des erreurs
  de transport ;
- réponses temporaires : droits `0700`/`0600` et nettoyage à la fermeture ;
- éditeur de ligne : saisie, retour arrière, UTF-8 fragmenté, historique,
  Ctrl-C, Ctrl-D, collage long sans duplication du prompt, notification avec
  redessin complet depuis l'origine du prompt et raccourcis d'édition ;
- addon Python : compilation syntaxique et test d'intégration avec un vrai
  `mitmdump`.

Le test d'intégration réel a été exécuté avec mitmproxy **12.3.0** et curl
**8.14.1** dans un environnement virtuel temporaire. mitmproxy n'est pas une
dépendance du module Go. Si `mitmdump` est absent, les tests d'intégration sont
ignorés ; le reste de la suite passe sans mitmproxy.

Les scripts d'installation ont aussi été vérifiés manuellement : compilation dans
un préfixe temporaire, exécution du binaire depuis un autre répertoire,
refus d'écraser un fichier personnel sans `--force`, puis désinstallation avec
contrôle d'empreinte.

Le shell courant de l'environnement de validation est **zsh**. La commande
installée a été vérifiée depuis `/tmp` avec :

```sh
zsh -lic 'cd /tmp && command -v crl && crl --version'
```

Fish n'était pas installé : `scripts/install-fish-path.sh` a seulement été
contrôlé syntaxiquement. Il reste disponible pour une machine équipée de Fish,
mais il n'est pas nécessaire sous zsh.

Tests d'intégration concernés :

```sh
go test ./internal/mint -run 'TestAddonPythonSyntax|TestRealMitmdumpCapture' -v
go test ./internal/shell -run TestRealMintAutoProxyInShell -v
```

Le second vérifie le chemin complet : démarrage réel de Mint par le shell,
`crl.launch` sans proxy explicite, passage effectif du trafic loopback par le
proxy, capture de la requête et de la réponse, puis arrêt sans perte de
l'historique.

## Structure du projet

```text
cmd/crl/                 point d'entrée, options et variables d'environnement
internal/display/        masquage et rendu terminal
internal/response/       analyse des réponses, JSON/HTML, erreurs curl
internal/request/        état du constructeur, import, priorité proxy, args curl
internal/mint/           gestionnaire mitmdump, pont local, addon, historique
internal/shell/          dispatch des commandes, éditeur de ligne, notifications
internal/termstyle/      palette ANSI commune et détection du terminal
scripts/                 installation, désinstallation, PATH Fish
```

Fichiers importants :

| Fichier | Contenu |
|---|---|
| `internal/mint/manager.go` | cycle de vie du proxy, disponibilité, arrêt du groupe de processus |
| `internal/mint/bridge.go` | pont HTTP loopback authentifié et validation des événements |
| `internal/mint/addon.py` | addon mitmproxy embarqué, capture et envoi asynchrone |
| `internal/mint/history.go` | stockage mémoire, association, filtre, sélection, éviction |
| `internal/request/builder.go` | constructeur, import et génération des arguments curl |
| `internal/display/mask.go` | masquage des secrets et rendu des corps Mint |
| `internal/response/response.go` | en-têtes finaux, corps JSON/HTML, métadonnées et erreurs curl |
| `internal/termstyle/termstyle.go` | palette ANSI commune, détection et styles de statut |
| `internal/shell/shell.go` | commandes `crl.*` et `mint.*`, aide, confirmations |
| `internal/shell/editor.go` | éditeur de ligne brut et redessin du prompt |
| `scripts/install.sh` | compilation et installation vérifiée dans `~/.local/bin` |
| `scripts/uninstall.sh` | désinstallation avec contrôle d'empreinte |
| `scripts/install-fish-path.sh` | configuration du PATH universel de Fish |

## Dépannage

**`go: downloading go1.23 ... toolchain not available`**

CRL-CLI demande Go 1.22 ou plus récent. Vérifiez la version active avec
`go version`. Si elle est antérieure à 1.22, installez Go 1.22+ ; si elle est
déjà suffisante, vérifiez qu'un fichier `go.work` ou une variable
`GOTOOLCHAIN` ne force pas une autre version :

```sh
go version
go env GOTOOLCHAIN GOWORK
```

**`compile: version "go1.22.2" does not match go tool version "go1.23.6"`**

Le compilateur et la bibliothèque standard proviennent de deux installations Go
différentes. Changer la ligne `go` de `go.mod` ou vider le cache ne corrige pas
un `GOROOT` qui pointe vers une ancienne installation. Le script d'installation
vérifie maintenant ce décalage et affiche une erreur explicite. Vérifiez les
chemins avec :

```sh
type -a go
go version
go env GOROOT GOTOOLDIR GOCACHE
env | grep '^GO'
```

Sélectionnez ensuite le binaire et le `GOROOT` d'une même installation. Avec
asdf, installez la version requise puis donnez son binaire au script :

```sh
asdf install golang 1.23.6
GOROOT=/chemin/vers/goroot GO=/chemin/vers/go/bin/go sh scripts/install.sh
```

Remplacez les deux chemins par ceux de l'installation asdf. `go version` et
`go env GOROOT` doivent désigner la même version de Go.

**`mitmdump introuvable dans PATH`**

Installez mitmproxy, puis vérifiez `mitmdump --version`. Vous pouvez aussi
définir `MINT_MITMDUMP=/chemin/absolu/vers/mitmdump`.

**`port 127.0.0.1:8080 indisponible`**

Choisissez un autre port :

```text
mint.proxy.port 8081
mint.proxy on
```

**Mint démarre mais aucune capture n'apparaît**

Vérifiez que le trafic passe bien par `http://127.0.0.1:<port>`. Pour du trafic
loopback, utilisez `--noproxy ""` avec un curl manuel, ou laissez `crl.launch`
le faire automatiquement. Vérifiez aussi qu'aucun `crl.proxy` explicite ne
détourne le trafic.

**Erreur TLS avec HTTPS**

Configurez le CA de mitmproxy avec `crl.ca <fichier.pem>`. La vérification TLS
reste active ; n'utilisez pas d'option désactivant la vérification.

**Le proxy s'arrête de manière inattendue**

`mint.proxy status` affiche la dernière erreur et les dernières lignes utiles de
`mitmdump`. Corrigez la configuration, puis relancez `mint.proxy on` : le shell
reste utilisable après l'échec.

**Le shell affiche des notifications pendant la saisie**

C'est le comportement attendu : les notifications Mint sont insérées au-dessus
du prompt sans perdre le texte en cours. Les caractères de contrôle sont filtrés
avant affichage.

## Licence

MIT. Voir [LICENSE](LICENSE).

## Mise à jour (session de stabilisation)

- Prompt mono-ligne `crl [GET] [Mint: ON] [Sel: N] >` ; la méthode devient `[Actif: GET]` avec une sélection pour éviter toute ambiguïté.
- Notifications Mint : pas d'écriture concurrente depuis le pont/goroutines pendant la saisie ; un seul lecteur `stdin`.
- Sélection Mint et constructeur actif indépendants ; `crl.show` détaille toutes les captures sélectionnées ; modifications ciblées stockées en copies de travail, captures originales inchangées.
- Édition JSON multiligne intégrée sans `$EDITOR`/`$VISUAL` (`:end`, `:cancel`), préservation du corps en cas d'erreur/annulation. Ajout de `crl.json set`/`delete` pour modifier/supprimer un champ précis en préservant le reste.
- `crl.body` multiligne intégrée. Corps envoyé fidèlement (gestion Content-Length/`--data-binary` via stdin), pas de réutilisation aveugle après modification.
- Rejeu parallèle avec sections et résumé par identifiant ; erreurs HTTP distinctes des erreurs réseau ; `mint.replay.body <id> [offset]` consulte le corps jusqu'à 16 Mio.

### Tests réellement exécutés
- `gofmt -w .`
- `go test ./...`
- `go vet ./...`
- `go test -race ./...`
- `go build ./cmd/crl`
