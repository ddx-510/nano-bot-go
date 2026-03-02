---
name: standup
description: Generate daily standup summary from git activity across all repos
always_on: false
---

# Daily Standup Generator

Generate a team standup by checking who committed what in the last 24 hours.

## Steps

### 1. Pull latest
```
exec("cd repos/my-repo && git pull --quiet")
```

### 2. Get commits per person
```
exec("cd repos/my-repo && git log --since='1 day ago' --format='%an|%s' --no-merges")
```

### 3. Format
- Group commits by person name from git log
- Summarize each person's commits into 1-2 bullet points per repo
- If someone had no commits, don't list them

## Output Format

```
Team Standup — {date}

{Person Name}
  - my-repo: {summary of their commits}

{Person Name}
  - my-repo: {summary of their commits}

Activity: {N} commits across {repos}
```
