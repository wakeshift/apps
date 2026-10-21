# Slackline Privacy Policy

*Draft for legal review · Last updated [date]*

Slackline is made by Clawvisor Inc. ("we", "us") and runs on Wakeshift. This notice is part of [Wakeshift's Privacy Policy]({wakeshift}/privacy). It says what Slackline collects, who sees it and how long it's kept. Who we are, why we use data, your rights, where data is stored and how we tell you about changes are all in Wakeshift's policy. Contact: support@clawvisor.com.

## What Slackline collects

- **Your Slack workspace:** its ID and name, who added Slackline and when, and the token Slack gives Slackline to read and post, stored encrypted.
- **You, when you sign in with Slack:** your Slack user ID and name, your workspace's name, your direct message channel with Slackline, and when you signed in.
- **Your agents:** the name you gave each one, the name its program gives itself (for example "Claude Code"), when you approved it and whether you removed it. The keys that let it connect are stored only as hashes.
- **Where each agent listens:** its thread in your direct messages, the channels you chose (their IDs and names, whether it's woken by every message or only by mentions, and whether only you or anyone there can reach it), and the threads it posted in.
- **A log of what you did:** connecting, renaming and removing agents, setting and removing channel rules, and signing in. It never holds message text.

**Messages.** Slack sends Slackline the messages in your direct messages with Slackline and in the channels Slackline has been added to. Slackline doesn't store them. It passes each message an agent is allowed to receive to Wakeshift, which holds it until the agent receives it, and drops the rest. When your agent replies, Slackline posts the reply to Slack and doesn't keep it.

Slackline sets two cookies: `slackline_session` keeps you signed in to your Slackline page, and `slackline_state` completes signing in or adding Slackline to Slack, for 10 minutes.

## Who sees it

- **Your agents, and their providers** (for example Anthropic or OpenAI), for the messages delivered to them.
- **The people in a channel** where your agent posts.
- **Slack**, which handles everything in Slack under its own privacy policy.
- **Google Cloud**, which hosts Slackline.

## How long it's kept

- **Messages** delivered to your agent stay on Wakeshift until Slackline is removed from your Slack workspace. Removing it has Wakeshift forget each person there, which deletes everything it holds for them, their agents' messages included.
- **An agent's keys and channel rules** are deleted when you remove the agent.
- **Everything else**, including the log of what you did, is kept while Slackline is in your Slack workspace. We may delete it sooner. Removing Slackline from Slack deletes all of it, and has Wakeshift delete what it holds for that workspace's people.
- **Logs** of requests (never message text or tokens) are kept 30 days.

See also the [Slackline Terms of Service](/terms).
