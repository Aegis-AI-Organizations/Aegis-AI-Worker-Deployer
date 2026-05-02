# 🏗️ Architecture du Deployer : Moteur de Rendu de Jumeau Numérique

Le **Worker Deployer Aegis AI** est le "Moteur de Réplication" de la plateforme. Écrit en **Go** pour son intégration native avec Kubernetes, il est responsable de la reconstruction de répliques isolées et haute fidélité de l'infrastructure client pour permettre des tests offensifs en toute sécurité.

---

## 🏗️ Principes de Conception de Base

Le Worker Deployer est conçu pour la **précision**, l'**isolement** et la **vitesse** :

1. **Traduction Graphique-vers-Manifeste** : Reconstitue l'état de l'infrastructure en interrogeant le graphique d'attaque **Neo4j** et en générant les manifestes Kubernetes et les politiques réseau correspondants.
2. **Bac à Sable Déterministe** : Provisionne des environnements éphémères dans des namespaces `sandbox-*` dédiés, garantissant que chaque jumeau numérique est une réplique propre et isolée de la cible.
3. **Orchestration Interne** : Dirigée par le `Aegis-AI-Brain` via **Temporal**, garantissant que le provisionnement de "Zones de Mission" complexes est résilient et fiable.

---

## 🔐 Sécurisation et Délimitation du Bac à Sable

Étant donné que ces workers déploient une infrastructure potentiellement vulnérable, ils mettent en œuvre des frontières de sécurité extrêmes.

- **Isolement du Noyau (gVisor)** : Chaque composant du jumeau numérique est provisionné en utilisant la classe de runtime **gVisor** (`runsc`), offrant un isolement fort au niveau des appels système depuis l'hôte sous-jacent.
- **Micro-segmentation (Cilium)** : Applique automatiquement des **Politiques Réseau Cilium** (Cilium Cluster-wide Network Policies) strictes au bac à sable, empêchant tout mouvement latéral involontaire vers le cœur Aegis interne ou d'autres bacs à sable.
- **Segmentation RBAC** : Le worker fonctionne avec un **ServiceAccount** restreint, autorisé uniquement à gérer les ressources au sein des namespaces de bac à sable explicitement désignés.

---

## 🌊 Mise à l'Échelle Dynamique (KEDA)

Le pool Deployer est géré par **KEDA** (Kubernetes Event-Driven Autoscaling) pour gérer les déploiements de campagnes en parallèle :

- **Mise à l'Échelle Réactive à la Demande** : Ajuste le nombre de workers en fonction du nombre de tâches "Déploiement" dans la file d'attente Temporal.
- **Scale-to-Zero (Mise à l'échelle vers zéro)** : Lorsqu'aucun déploiement n'est actif, le pool se réduit à **0 réplica**, optimisant l'utilisation des ressources.

---

## 🛰️ Logique de Déploiement

Le worker gère la traduction de :
- **Topologies de Service** : Groupes d'autoscaling, déploiements et ensembles avec état (stateful sets).
- **Métadonnées Réseau** : Règles d'ingress, équilibreurs de charge et entrées DNS internes.
- **Contextes de Sécurité** : Réplication du niveau de privilège exact et des contraintes d'exécution de la cible réelle.

```mermaid
graph TD
    Neo4j[(Neo4j Graph)] -- "Données de Topologie" --> Deployer[Worker Deployer (Go)]
    Deployer -- "Rendu" --> K8s[Cluster K8s Cible]
    K8s -- "Déployer" --> Sandbox[Jumeau Numérique Isolé]
    Sandbox -- "Prêt" --> Brain[Orchestrateur Brain]
```

---

*Infrastructure Aegis AI & Jumeaux Numériques — 2026*
