# SPEC — Aquifer : miroir APT distribué, cache adressé par contenu

> À lire intégralement avant d'écrire la moindre ligne. Commence par me proposer un plan
> d'implémentation découpé en étapes, attends ma validation, puis implémente étape par étape.
> Tout le code, les commentaires, les noms d'identifiants, les messages de commit et la
> documentation sont en **anglais US**. Cette spec est en français, ne la traduis pas dans le repo.

---

## 1. Objectif

Remplacer un miroir APT monolithique (aptly + rsync + nginx sur une seule machine) par un
système distribué :

- un **master** privé, sans flux entrant, qui publie vers un stockage objet ;
- N **edges** sans état durable, qui servent les clients `apt` depuis un cache disque local
  plafonné, alimenté depuis le stockage objet.

Le projet s'appelle **Aquifer** : une nappe (le stockage objet) et des puits qui pompent
dessus (les edges). Un binaire unique, `aquifer`, avec des sous-commandes.

Problèmes résolus : consommation disque, SPOF (le master n'est pas dans le chemin de service),
pic de charge (N edges + coalescence des téléchargements).

## 2. Volumétrie réelle — à garder en tête pour tout dimensionnement

Mesures sur la publication existante :

| | Valeur |
|---|---|
| Fichiers totaux | 794 |
| Taille totale | 17,4 Gio |
| Taille moyenne | ~22 Mio, avec des paquets à 60–90 Mio |
| Métadonnées (`dists/**`, 24 publications) | **6,8 Mio** |
| Working set observé sur les logs nginx | ~8 Gio de volume distinct |
| Budget de cache cible par edge | **5 Gio** |

Conséquences directes :

- 794 entrées de manifeste : **pas de base de données**. Un TSV compressé zstd (~25 Kio)
  chargé dans une `map[string]entry` coûte 200 Kio de heap. Aucun SQLite, bbolt, CDB ou index
  sur disque — voir §9 pour le format exact.
- 6,8 Mio de métadonnées : les épingler est gratuit.
- 5 Gio pour ~8 Gio de working set : il y aura une **pression d'éviction réelle**. La politique
  de cache est un sujet de premier plan, pas un garde-fou.

Le dépôt est **multi-repo** : plusieurs publications indépendantes coexistent
(`debian/bookworm`, `debian/trixie`, `ubuntu/{bionic,focal,jammy,noble}`, `salt`, `all`, et une
publication à la racine). Chacune a sa propre révision, sa propre bascule, son propre manifeste.
Le concept de `repo` est de première classe dans tout le code.

## 3. Le principe qui structure tout : rien n'est jamais invalidé

Dans un dépôt Debian, `pool/**` est immuable par construction. Seul `dists/**` mute.

On exploite ça en adressant **tout** par contenu :

```
s3://<bucket>/<prefix>/blobs/sha256/<ab>/<cd>/<full-hex-hash>   # immuable, dédupliqué
s3://<bucket>/<prefix>/manifests/<repo>/<revision>.tsv.zst      # immuable
s3://<bucket>/<prefix>/refs/<repo>/current                      # pointeur, muté atomiquement
```

Un fichier modifié produit un nouveau hash, donc un nouveau blob, donc une nouvelle entrée de
manifeste. L'ancien blob en cache local devient non référencé et sort par LRU.

**Il ne doit exister nulle part dans le code une notion de « purge », « invalidate » ou « TTL de
cache ».** Si tu te retrouves à en écrire une, c'est que le design a dérivé — arrête-toi et
signale-le moi.

## 4. `aquifer publish` (tourne sur le master)

Entrée : un ou plusieurs répertoires de publication produits par `aptly publish`.

1. Parse `dists/**/Release` pour les chemins et SHA256 des index.
2. Parse les `Packages` (et `Sources` si présents) pour les chemins et SHA256 de `pool/`.
   **Ne re-hashe rien** : aptly a déjà tout calculé. Ne calcule un hash que pour un fichier
   n'apparaissant dans aucun index (clés GPG servies en statique, par exemple — traite-les
   comme des blobs ordinaires, aucun cas particulier).
3. `ListObjectsV2` sur le préfixe `blobs/` pour connaître l'existant. À 794 objets c'est une
   requête paginée en une seconde : **aucun index local à maintenir, aucun état sur le master**.
4. Upload les blobs manquants en parallèle (worker pool, concurrence configurable).
5. Construit et upload `<revision>.tsv.zst` (format en §9).
6. **En dernier et seulement en dernier**, écrit `refs/<repo>/current`. C'est le commit
   atomique : si le job meurt avant, il ne s'est rien passé (des blobs orphelins que le GC
   ramassera).

Format de révision : `<unix-timestamp>-<short-uuid>`, triable lexicographiquement.

`aquifer gc` : mark-and-sweep des blobs non référencés par les N dernières révisions
retenues, tous repos confondus. **Grâce period obligatoire** : ne jamais supprimer un blob dont
le `LastModified` a moins de 24 h, sinon course avec une publication en cours.

## 5. `aquifer serve` (edge, serveur HTTP)

- Poll `refs/<repo>/current` pour **chaque repo** toutes les 15 s avec `If-None-Match`. Sur
  changement : télécharge le nouveau manifeste, puis swap atomique du pointeur en mémoire
  (`atomic.Pointer[Manifest]`, lectures lock-free sur le chemin chaud).
- Sert `GET` et `HEAD`. Résolution : `path` → repo → manifeste → `sha256` → cache ou fetch.
- **Fenêtre de révisions** : garde les K derniers manifestes par repo (défaut 5). Les chemins
  `pool/**` sont résolus contre l'**union** des révisions retenues ; les chemins `dists/**`
  strictement contre la révision courante. Ça évite un 404 quand un client fait `apt update`
  sur un edge et `apt install` sur un autre pendant une bascule.
- Sert avec `http.ServeContent` : `Range`, `Last-Modified`, `ETag` (le hash) gratuits. Le
  support des `Range` n'est pas optionnel — avec des objets de 90 Mio, la reprise sur coupure
  est un besoin réel.
- Support de `dists/**/by-hash/SHA256/<hash>` : résolution directe sans passer par le manifeste.

## 6. Politique de cache — sélection par motifs glob

Deux listes de motifs indépendantes, configurables, évaluées contre le **chemin de service
complet** (préfixe de repo inclus) :

```yaml
cache:
  max_size: 5GiB          # LRU budget, excludes pinned entries and in-flight temp files
  pinned_max_size: 1GiB   # safety cap: refuse to start if pinned set exceeds this
  temp_reserve: 3GiB      # disk headroom for in-flight downloads, outside max_size

  # Never evicted, always served locally.
  pinned:
    - "**/dists/**"
    - "dists/**"

  # Fetched in the background on every new revision. Pinned patterns are
  # implicitly prefetched; list additional ones here.
  prefetch:
    - "**/dists/**"
    - "dists/**"
```

Règles de sémantique :

- **Épinglé** : le blob est téléchargé au chargement de chaque révision et **jamais évincé**.
  Les entrées épinglées sont comptabilisées séparément et **ne consomment pas `max_size`**.
- **Préchargé** : le blob est téléchargé en tâche de fond après la bascule de révision, puis
  géré normalement par le LRU. Sert à préparer le cache sans le figer.
- Un motif épinglé est implicitement préchargé.
- Les blobs ni épinglés ni préchargés sont récupérés paresseusement, à la demande.

Contraintes d'implémentation :

- Syntaxe glob avec support de `**` : utilise `github.com/bmatcuk/doublestar/v4` (pur Go).
  `path/filepath.Match` ne suffit pas, il ne connaît pas `**`.
- **Validation au démarrage et à chaque bascule de révision** : compte les objets et les octets
  correspondant à chaque motif, et logue le résultat
  (`pinned: 2 patterns, 312 objects, 6.8 MiB`). Si le total épinglé dépasse `pinned_max_size`,
  refuse de démarrer avec un message explicite. Un motif trop large (`**`) doit échouer vite et
  bruyamment, pas remplir le disque en silence.
- Expose `aquifer_cache_pinned_bytes` et `aquifer_cache_pinned_objects` en métriques.
- Les motifs qui ne correspondent à rien génèrent un `WARN` au démarrage (typiquement une
  faute de frappe dans un préfixe de repo).

Éviction :

- LRU strict sur le segment non épinglé, **jamais synchrone dans le chemin de requête**. Deux
  seuils : à 100 % de `max_size`, une goroutine de fond évince jusqu'à 90 %. Sinon tu prends
  des `unlink` de 90 Mio en plein milieu d'une réponse HTTP.
- Les fichiers temporaires des téléchargements en cours **ne comptent pas** dans `max_size` :
  ils ont leur propre réserve (`temp_reserve`). Avec 20 téléchargements concurrents à 90 Mio,
  c'est 1,8 Gio en vol qui ne doivent pas provoquer d'éviction.

## 7. Coalescence des téléchargements — exigence centrale

**Un edge ne télécharge jamais deux fois le même blob simultanément. Point.** À un instant T,
pour un hash donné, il existe au plus un `GET` vers le stockage objet. Tous les autres
demandeurs streament depuis ce téléchargement unique ou attendent.

C'est l'exigence la plus importante de la spec. Il n'y a **aucune échappatoire** : les clients
sont derrière un filtrage domaine/IP et ne peuvent joindre que l'edge — pas de redirection
possible vers le stockage objet, pas de délestage. Sans coalescence, 40 VM qui collisionnent
sur un paquet de 90 Mio absent du cache, c'est 3,6 Gio d'egress au lieu de 90 Mio.

### Chemin de code unique pour tous les miss

Tout miss, qu'il finisse en cache ou non, emprunte le **même** mécanisme :

1. Le premier demandeur devient le **leader**. Il ouvre le `GET`, écrit dans un fichier
   temporaire, incrémente un compteur `written` (atomique) au fil de l'eau, et calcule le
   SHA256 en streaming.
2. Les demandeurs suivants deviennent **followers**. Chacun ouvre son propre descripteur sur le
   fichier temporaire et lit jusqu'à `written`. Quand ils rattrapent le leader, ils attendent
   une notification (`chan struct{}` remplacé et fermé à chaque avancée — préférable à
   `sync.Cond` car ça compose avec `ctx.Done()`).
3. À la complétion : vérification du SHA256, `fsync`. **C'est seulement là que la politique
   d'admission décide** : `rename` vers le cache, ou `unlink` du temporaire. Le résultat servi
   aux clients est identique dans les deux cas.
4. En cas d'erreur (réseau, checksum, disque plein) : tous les followers reçoivent la même
   erreur, le temporaire est supprimé, et le blob n'entre jamais dans le cache.
5. Un demandeur qui arrive après la complétion est servi depuis le cache, ou déclenche un
   nouveau téléchargement si le blob n'a pas été admis. Attention à la course : l'enregistrement
   dans la map doit précéder la lecture de l'état.

Ce chemin unique est ce qui rend l'objet non caché coalesçable — sans fichier temporaire, les
followers n'auraient rien à suivre.

### Contraintes non négociables

1. **Le contexte du leader ne doit pas être celui de sa requête HTTP.** Utilise
   `context.WithoutCancel(ctx)` ou un contexte de fond avec son propre timeout. Sinon le premier
   client qui fait `Ctrl-C` tue le téléchargement des 39 autres. C'est le bug classique de ce
   pattern et je le considère comme bloquant.
2. Compteur de références : l'entrée est nettoyée quand le leader a terminé **et** que tous les
   followers sont partis.
3. Une requête `Range` sur un blob en cours de téléchargement : ne l'implémente pas, sers-la en
   attendant la complétion. Ne bypasse jamais la coalescence.
4. Le code doit passer `go test -race` avec 100 goroutines concurrentes sur le même hash.

### Tests exigés

- 100 demandeurs concurrents sur un blob absent → **exactement un** `GET` sur le backend, tous
  reçoivent le contenu correct.
- Idem pour un blob que la politique d'admission refusera de cacher → toujours un seul `GET`.
- Le leader annule son contexte HTTP → les followers reçoivent quand même le blob complet.
- Le backend renvoie un contenu dont le SHA256 ne correspond pas → tous reçoivent une erreur,
  rien n'est écrit dans le cache.
- Erreur d'écriture disque en cours de route → même comportement.
- Un demandeur arrivant à 99 % de progression obtient le fichier complet.
- Deux hashes différents en parallèle ne se bloquent pas mutuellement.

## 8. Hors périmètre

- **Pas d'authentification.** Le serveur sert tout le monde. Prévois une couture propre :
  une interface `Authorizer` avec une implémentation `AllowAll` par défaut, branchée en
  middleware. Ne construis rien d'autre autour.
- Pas de TLS côté application (le reverse proxy s'en charge).
- **Pas de redirection vers le stockage objet.** Exclue par le filtrage réseau des clients.
- Pas d'autres formats (RPM, PyPI, OCI). Debian/Ubuntu uniquement.
- Pas de fetch pair-à-pair entre edges.
- Pas d'endpoints snapshot/rollback (mais garde le modèle de données compatible).

## 9. Stack et contraintes techniques

- Go stable le plus récent. Modules, pas de vendoring.
- **`CGO_ENABLED=0` obligatoire.** Aucune dépendance ne doit l'exiger.
- **Manifeste : TSV compressé zstd.** Une ligne par entrée, trois colonnes séparées par des
  tabulations, triées par chemin :

  ```
  <path>\t<sha256-hex>\t<size-bytes>
  ```

  Précédé de lignes d'en-tête commençant par `#` portant les métadonnées
  (`# format_version`, `# repo`, `# revision`, `# created_at`).

  Justification : à 794 entrées le lookup n'est jamais le goulot, et un format texte
  s'inspecte avec `zstdcat manifest.tsv.zst | grep`, se diffe entre deux révisions avec
  `diff`, et reste lisible dans dix ans sans outil. Une base de données ferait payer un moteur
  SQL complet pour ce qui est un `map[string]entry`.

  Côté `publish` : écriture en streaming, `github.com/klauspost/compress/zstd` (pur Go).
  Tri lexicographique obligatoire — le manifeste doit être **déterministe**, deux publications
  du même contenu produisent le même fichier octet pour octet.

  Côté `serve` : parsing en streaming avec `bufio.Scanner`, chargement dans une
  `map[string]entry`. Rejette le fichier si `format_version` est inconnu, si une ligne est
  malformée, ou si les chemins ne sont pas triés — un manifeste corrompu doit échouer au
  chargement, jamais être partiellement accepté.
- Client S3 : `aws-sdk-go-v2` ou `minio-go`. La cible peut être du Swift via sa couche de
  compatibilité S3 : endpoint personnalisable, path-style adressable, région optionnelle.
- Coalescence : implémentation maison (voir §7), pas `golang.org/x/sync/singleflight` qui ne
  sait pas partager un flux en cours.
- **Streaming systématique.** Aucun blob ne doit jamais être entièrement bufferisé en mémoire.
  `io.Copy` avec un `sync.Pool` de buffers.
- Configuration : fichier YAML, surchargeable par variables d'environnement puis par flags.
- Logs structurés via `log/slog`, JSON en production.
- Zéro framework web : `net/http` et `http.ServeMux` suffisent.

## 10. Métriques Prometheus

Endpoint `/metrics` sur un port d'administration séparé.

- `aquifer_cache_requests_total{class="pinned|pool", result="hit|miss|error"}` — la
  ventilation par classe est indispensable : un hit global de 85 % peut cacher un `dists` à
  100 % et un `pool` à 40 %, et les deux situations appellent des actions opposées.
- `aquifer_fetch_coalesced_readers_total` — combien de téléchargements ont été économisés.
  C'est la métrique qui prouve que §7 fonctionne.
- `aquifer_fetch_inflight`
- `aquifer_cache_bytes`, `aquifer_cache_evictions_total`, `aquifer_cache_pinned_bytes`,
  `aquifer_cache_pinned_objects`
- `aquifer_manifest_revision_info{repo, revision}` — permet d'alerter sur la divergence entre
  edges.
- `aquifer_manifest_age_seconds{repo}`
- `aquifer_release_valid_until_seconds{repo, suite}` — secondes restantes avant expiration du
  `Valid-Until` de l'`InRelease`. Alerte de fraîcheur la plus fiable : à zéro, `apt` refuse le
  dépôt côté client.
- Latence en histogramme, ventilée par classe.

Optionnel mais utile pour arbitrer le budget de 5 Gio : un **shadow cache** qui ne stocke que
les hashes (pas les données) et simule des plafonds alternatifs, exposé en
`aquifer_cache_shadow_hit_ratio{size="10GiB"}`. Coût mémoire dérisoire, permet de décider sur
des faits plutôt qu'à l'intuition.

Endpoints `/healthz` (le processus vit) et `/readyz` (tous les manifestes chargés, épinglés
présents, aucun `Valid-Until` expiré).

### `aquifer ping`

Sous-commande de diagnostic, conçue pour être appelée depuis un `HEALTHCHECK` Docker, un check
Consul ou un script d'exploitation. Elle interroge `/readyz` sur l'instance locale et sort avec
le code `0` si tout va bien, `1` sinon.

Contraintes :

- Résout l'adresse cible dans cet ordre : flag `--addr`, variable `AQUIFER_ADMIN_ADDR`, le
  fichier de configuration s'il est lisible, puis `http://127.0.0.1:<port admin par défaut>`.
  Elle doit fonctionner **sans aucun argument** dans le conteneur.
- Timeout court, 2 s par défaut : un healthcheck qui pend est un healthcheck cassé.
- Silencieuse en cas de succès. En cas d'échec, une ligne sur `stderr` indiquant *quelle*
  condition de `/readyz` a échoué — c'est ce qu'on lit dans `docker inspect` à 3 h du matin.
- Un flag `--verbose` affichant le détail JSON de `/readyz` (révisions par repo, taille du
  cache, objets épinglés), pour l'usage interactif.
- Aucune dépendance au reste du runtime : pas de chargement de manifeste, pas de connexion S3,
  pas d'ouverture du cache.

## 11. Image Docker

**Interdiction formelle d'Alpine**, sous quelque forme que ce soit — ni comme base, ni comme
étage de build.

- Multi-stage : builder `golang:<version>-bookworm`, runtime
  `gcr.io/distroless/static-debian12:nonroot`.
- Build : `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w"`. Binaire statique.
- Tourne en non-root (UID 65532, fourni par le tag `:nonroot`).
- Racine en lecture seule ; seul le volume de cache est inscriptible.
- Multi-arch `linux/amd64` et `linux/arm64` via buildx.
- Distroless n'a **pas de shell**. Deux conséquences :
  - le `HEALTHCHECK` ne peut pas appeler `curl` ou `wget` ;
  - la **forme shell** de `HEALTHCHECK CMD` est inutilisable, puisqu'elle enveloppe la commande
    dans `/bin/sh -c`. Il faut la **forme exec** :

  ```dockerfile
  ENTRYPOINT ["/aquifer"]
  CMD ["serve"]
  HEALTHCHECK --interval=15s --timeout=3s --start-period=60s --retries=3 \
      CMD ["/aquifer", "ping"]
  ```

  Le `start-period` généreux laisse le temps au préchargement des motifs épinglés.
- `.dockerignore` strict. Labels OCI (`org.opencontainers.image.*`) renseignés.
- Génération d'un SBOM, build reproductible si possible.
- Jamais de tag `latest` dans les exemples de documentation.

## 12. Documentation à produire

Dans `docs/`, en anglais, avec des exemples de configuration **complets et testables**, pas des
fragments. Chaque page contient une section « vérification » avec les commandes `apt` à lancer
pour valider le déploiement.

### `docs/deploy-nginx.md`

Reverse proxy devant l'edge : terminaison TLS, `proxy_pass`. Points à traiter explicitement :

- `proxy_buffering off` pour les gros fichiers (sinon nginx met en tampon des blobs entiers).
- **Ne pas activer `proxy_cache`** — l'edge gère déjà son cache, un double cache introduit de
  l'incohérence et casse la comptabilité du plafond. Explique pourquoi dans la doc.
- `proxy_read_timeout` généreux : un follower peut légitimement attendre plusieurs minutes.
- Transmission de `X-Forwarded-For` / `X-Real-IP`.

### `docs/deploy-caddy.md`

Caddyfile complet avec TLS automatique. Mêmes points d'attention (pas de cache, timeouts, flush
du buffer de réponse via `flush_interval -1`).

### `docs/deploy-nomad.md`

Fichier de job HCL complet, enregistrement du service dans Consul avec health check HTTP sur
`/readyz`, tags Traefik pour l'exposition, stanza `update` avec `max_parallel = 1` et
`min_healthy_time` suffisant pour laisser le préchargement se faire avant de basculer
l'instance suivante, injection des credentials S3 via un template Vault.

### Autres

- `README.md` : quoi, pourquoi, démarrage rapide avec MinIO en compose.
- `docs/architecture.md` : modèle de données, mécanisme de révision, coalescence.
- `docs/configuration.md` : référence complète du YAML, avec une section détaillée sur les
  motifs `pinned` et `prefetch` — c'est le levier de réglage principal de l'exploitant.
- `docs/operations.md` : GC, supervision, que faire quand un edge diverge, réglage du plafond
  de cache à partir des métriques.

## 13. Arborescence attendue

```
cmd/aquifer/           # single static binary: serve, publish, gc, ping
internal/cli/          # subcommand wiring
internal/manifest/     # build, load, revision window
internal/blobstore/    # S3/Swift client, content-addressed layout
internal/cache/        # LRU, pinning, glob matching, admission
internal/fetch/        # concurrent download sharing — the critical part
internal/debian/       # Release / Packages parsing
internal/server/       # HTTP handlers, middleware, metrics
docs/
deploy/
```

## 14. Méthode de travail

- Propose le plan avant de coder. Attends ma validation.
- `internal/fetch` en TDD : les tests de §7 avant l'implémentation. C'est le cœur du projet,
  traite-le en premier et donne-lui le soin qu'il mérite.
- Commits atomiques, messages conventionnels, en anglais.
- `golangci-lint` configuré et propre. `go vet` et `go test -race` dans la CI.
- Test d'intégration final, en conteneur : `debootstrap` puis `apt update && apt install` contre
  un edge, pour chaque suite et chaque architecture configurée. C'est le seul test qui prouve
  vraiment que ça marche.
- N'anticipe pas les besoins listés en §8. Si tu penses qu'une abstraction supplémentaire est
  justifiée, demande-moi avant de l'introduire.
