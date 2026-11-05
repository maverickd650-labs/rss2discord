# Quick Start

See [README.md](../README.md) for full documentation. This is just the fastest path to a running feed.

## 1. Deploy the operator

Requires Kubernetes 1.29+ (the CRD uses CEL validation rules, GA in 1.29) and a Discord webhook URL.

```bash
helm install rss2discord-operator ./dist/chart \
  --namespace rss2discord-operator-system \
  --create-namespace

kubectl get pods -n rss2discord-operator-system
```

(Or `kubectl apply -f dist/install.yaml` if you'd rather skip Helm.)

## 2. Create a Discord webhook secret

```bash
kubectl create secret generic discord-webhook \
  -n default \
  --from-literal=url='https://discord.com/api/webhooks/YOUR_WEBHOOK_ID/YOUR_TOKEN'
```

## 3. Create a FeedGroup

`my-feedgroup.yaml`:

```yaml
apiVersion: rss2discord.maverickd650.dev/v1beta1
kind: FeedGroup
metadata:
  name: tech-news
  namespace: default
spec:
  discordWebhookSecretRef:
    name: discord-webhook
    key: url
  interval: "30m"
  retries: 3
  retryInterval: "5m"
  format: |
    **{{.Title}}**
    {{.Description}}
    [Read more]({{.Link}})
  feeds:
    - name: hackernews
      rssUrl: "https://news.ycombinator.com/rss"
      filter:
        keywords:
          - kubernetes
          - golang
        excludeKeywords:
          - "hiring"
    - name: reddit-golang
      rssUrl: "https://www.reddit.com/r/golang/.rss"
```

```bash
kubectl apply -f my-feedgroup.yaml
```

## 4. Check it's working

```bash
kubectl logs -n rss2discord-operator-system -l control-plane=controller-manager -f
kubectl describe feedgroup tech-news -n default
```

## Next

- Tweak `format` to change how messages look in Discord.
- Add a `filter` (regex, keywords, or negative `excludeKeywords`/`excludeRegex` vetoes) to a feed to cut down on noise.
- Set `embed.enabled: true` (with a `color`, `authorURL`, `footerIconURL`, etc.) to render entries as Discord embeds instead of plain text, or `forumThreadName` on a feed to post into a forum channel.
- See the [Schema Reference](schema.md) for full field documentation and [Configuration reference](../README.md#configuration-reference) in the README.
