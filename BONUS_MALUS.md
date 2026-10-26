# Suivi des bonus et malus

Fichier de suivi des points accordés aux tâches réalisées.

## Règles

- **Bonus** : points positifs accordés quand une tâche est bien réalisée.
- **Malus** : points négatifs en cas d'erreur, de régression ou de consigne non respectée.
- Chaque ligne indique la date, la tâche concernée, le type, les points et le motif.
- Le solde est mis à jour à chaque nouvelle entrée.

## Solde actuel

| Bonus | Malus | Solde |
|---:|---:|---:|
| +6 | 0 | **+6** |

## Historique

| Date | Tâche | Type | Points | Motif |
|---|---|---|---:|---|
| 2026-10-25 | Projet CRL-CLI / Mint | Bonus | +6 | Tout passe très bien après correction de l'affichage interactif |

## Modèle d'ajout

```text
| AAAA-MM-JJ | Description de la tâche | Bonus ou Malus | +N ou -N | Raison |
```

Puis mettre à jour le tableau « Solde actuel ».
