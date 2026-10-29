# Quick Start

See [README.md](README.md) for full documentation. This is just the fastest path to a running feed.

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
apiVersion: rss2discord.maverickd650.dev/v1alpha1
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
    - rssUrl: "https://news.ycombinator.com/rss"
      filter:
        keywords:
          - kubernetes
          - golang
    - rssUrl: "https://www.reddit.com/r/golang/.rss"
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
- Add a `filter` (regex or keywords) to a feed to cut down on noise.
- Set `embed.enabled: true` (with a `color`) to render entries as Discord embeds instead of plain text, or `forumThreadName` on a feed to post into a forum channel.
- See the [Configuration reference](README.md#configuration-reference) and [Troubleshooting](README.md#troubleshooting) sections in the README for everything else.
