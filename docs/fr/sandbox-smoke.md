# Validation smoke de la sandbox

Ce dépôt inclut une fixture de sandbox vulnérable canonique et un script de
smoke/debug pour valider que le deployer a bien produit un clone isolé et
joignable.

## Fichiers

- `examples/sandbox-topology.vulnerable-webapp.json` : payload de topologie pour
  l'application web vulnérable plus PostgreSQL.
- `examples/sandbox-request.vulnerable-webapp.json` : forme complète de la requête
  `CreateSandbox` avec `scan_id`, endpoint préféré et topologie.
- `scripts/sandbox_smoke.sh` : aide de validation locale pour les ressources
  Kubernetes, la joignabilité de l'endpoint, les events et le nettoyage optionnel.

## Forme attendue de la sandbox

La fixture déploie :

- `vulnerable-webapp` : point d'entrée HTTP sur le port de service `80`, port de
  conteneur `5000`.
- `postgres` : service PostgreSQL sur le port `5432`.
- `depends_on` : l'app déclare `postgres` comme dépendance, donc le deployer crée
  d'abord la charge de base de données.
- `wait_for` : l'app reçoit un init container qui attend `postgres:5432` avant de
  démarrer.
- `config_files` et `empty_dirs` : l'app reçoit une configuration montée et un
  stockage d'upload accessible en écriture.
- `stateful` et `headless` : PostgreSQL est rendu comme un StatefulSet avec une
  identité DNS stable.
- Mapping app-vers-DB via les variables `DATABASE_URL` et `POSTGRES_*`.
- URL de dépendance externe pointant vers `https://payments.example.test/api/v1`,
  qui doit être interceptée par la couche mock/DNS externe de la sandbox.

La boucle de pentest attendue est :

1. Le Brain envoie la requête de topologie au worker Deployer.
2. Le Deployer crée le namespace `aegis-war-room-<scan_id>`.
3. Le Deployer crée les workloads, services, egress default-deny et les services
   mock DNS/HTTP/HTTPS externes.
4. Le Brain seed PostgreSQL avec des données réalistes et `aegis-flag-1234`.
5. Le Worker Pentest exploite la SQLi et rapporte `loot_proof` et
   `exfiltrated_data`.

## Champs de fidélité supportés par le Deployer

Les workloads de topologie peuvent désormais exprimer ces contrôles de fidélité
Kubernetes :

- Démarrage : `command`, `args`, `working_dir`, `init_containers`.
- Démarrage des dépendances : `depends_on` pour l'ordre de création et `wait_for`
  pour les attentes TCP d'init-container telles que `postgres:5432`.
- Fichiers et stockage : `config_files`, `secret_files` et `empty_dirs` montés
  dans le conteneur du workload.
- Identité du workload : `stateful: true` rend un StatefulSet au lieu d'un
  Deployment.
- Services : `service.headless`, `service.type`, ports nommés et alias DNS dans le
  même namespace via `service.aliases`.
- Ressources : `resources.requests` et `resources.limits` Kubernetes.
- Sécurité : `security_context` du conteneur et `pod_security_context` du pod.
- Politique de readiness : `required: true` fait échouer la création de sandbox si
  ce workload ne devient jamais prêt.

Champs volontairement gérés hors de ce worker :

- La restauration de dump de base de données depuis MinIO/S3 relève des activités
  de seeding du Brain.
- Les réponses d'API externes riches en scénarios et la capture de trafic exigent
  que le runtime du mock externe persiste les logs de requêtes.
- L'exposition ingress/TLS en virtual-host exige une configuration
  d'ingress/contrôleur du cluster et doit être ajoutée via les manifests infra.

## Test smoke

Après création d'une sandbox, exécutez :

```bash
scripts/sandbox_smoke.sh --scan-id smoke-sqli-001
```

Si la réponse `CreateSandbox` a renvoyé un endpoint différent, passez-le
explicitement :

```bash
scripts/sandbox_smoke.sh \
  --scan-id smoke-sqli-001 \
  --endpoint http://vulnerable-webapp.aegis-war-room-smoke-sqli-001.svc.cluster.local:80
```

Pour inspecter un namespace arbitraire :

```bash
scripts/sandbox_smoke.sh --namespace aegis-war-room-scan-123
```

Pour nettoyer après inspection :

```bash
scripts/sandbox_smoke.sh --scan-id smoke-sqli-001 --cleanup
```

Le script refuse de supprimer les namespaces qui ne commencent pas par
`aegis-war-room-`.

## Ce à quoi ressemble un bon résultat

- Le JSON de topologie valide.
- Le namespace existe.
- Les network policies sont présentes.
- Les services incluent l'app, la base de données et le mock externe.
- L'ordre de dépendance crée les workloads base de données/cache avant les apps
  dépendantes lorsque `depends_on` est présent.
- Les Deployments deviennent disponibles ou affichent des erreurs actionnables.
- Les pods deviennent prêts ou affichent des erreurs actionnables.
- L'endpoint répond depuis l'intérieur du namespace.
- Les events ne montrent pas d'image pull non résolue, de mount refusé ou d'échec
  de scheduling.

## Débogage

Commandes utiles quand le script smoke échoue :

```bash
kubectl describe namespace aegis-war-room-smoke-sqli-001
kubectl get all -n aegis-war-room-smoke-sqli-001 -o wide
kubectl describe pod -n aegis-war-room-smoke-sqli-001 <nom-du-pod>
kubectl logs -n aegis-war-room-smoke-sqli-001 deploy/vulnerable-webapp
kubectl get events -n aegis-war-room-smoke-sqli-001 --sort-by=.lastTimestamp
```

Si les pods échouent en `ImagePullBackOff`, publiez ou retaguez l'image de l'app
vulnérable utilisée dans `examples/sandbox-topology.vulnerable-webapp.json`.
