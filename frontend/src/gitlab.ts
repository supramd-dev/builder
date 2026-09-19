// gitlabWebhooksURL turns a repository location (the site config's codeRepo,
// or the setup page's repository input) into the GitLab project's webhook
// integration page URL — the page the user would otherwise navigate to by
// hand (project → Settings → Webhooks). Returns "" when the repository is not
// an http(s) GitLab URL (a bare "group/project" path or an SSH remote gives
// no host to link to).
//
// Shared by the settings page's Webhook tab and the first-run setup page, so
// both point at the same place for the same repository.
export function gitlabWebhooksURL(codeRepo: string): string {
  let url = codeRepo.trim()
  if (!url.startsWith('http://') && !url.startsWith('https://')) return ''
  url = url.replace(/\/+$/, '').replace(/\.git$/, '')
  return `${url}/-/hooks`
}
