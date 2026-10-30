# Slackline Privacy Policy

*Last updated October 30, 2026*

This Slackline Privacy Policy ("Policy") describes the personal data that Slackline collects, who receives it and how long it is kept. Slackline is an app that Clawvisor Inc. ("we" or "us") makes, and it runs on Wakeshift. This Policy forms part of [Wakeshift's Privacy Policy]({wakeshift}/privacy), which describes who we are, why we process personal data and on what legal bases, your rights, where data is stored and how we notify you of changes. If your agent needs the `wakeshift` program, our [Connector Terms]({wakeshift}/connector-terms) govern that program, and [Section 4 of that Privacy Policy]({wakeshift}/privacy#the-wakeshift-program) describes the data it processes. You may contact us at support@wakeshift.ai.

## 1. Data Collected

- **Your Slack workspace:** its ID and name, who added Slackline and when, and the token that Slack gives Slackline to read and post, stored encrypted.
- **Your sign-in:** when you sign in with Slack, your Slack user ID and name, your Slack workspace's name, your direct message channel with Slackline, when you signed in, and when you confirmed that you understand where your agents' messages go.
- **Your agents:** the name you gave each agent, the name its agent program gives itself (for example, "Claude Code"), when you approved it and whether you removed it. The keys that allow it to connect are stored only as hashes.
- **Where each agent listens:** its thread in your direct messages; the channels you chose (their IDs and names, whether every message or only mentions reach the agent, and whether only you or anyone in the channel can reach it); and the threads in which it posted.
- **Slack profiles, briefly:** to determine who can reach an agent and to identify whose agent is posting, Slackline looks up people in Slack: their name, and whether they are a guest, from another organization or deactivated. Slackline keeps this information in memory for no more than 10 minutes and never stores it.
- **A log of your actions:** connecting, renaming and removing agents; setting and removing channel rules; signing in; and confirming where messages go. The log never contains message text.

Slackline sets two cookies: `slackline_session`, which keeps you signed in for up to 30 days, and `slackline_state`, which lasts 10 minutes and is used only to complete a sign-in with Slack. Neither is used for tracking.

Slackline counts visits to its home page, and a few actions on it, with PostHog, as Section 6 of [Wakeshift's Privacy Policy]({wakeshift}/privacy) describes. PostHog receives each page's address, with identifiers removed, and not your IP address. The analytics set no cookies, store nothing in your browser and do not record what you type.

## 2. Messages

2.1 Slack sends Slackline the messages in your direct messages with Slackline and in the channels to which Slackline has been added. Slackline never stores message text. It passes each message that one of your agents is permitted to receive to Wakeshift, and discards the rest. Messages from guests and from people outside your organization never reach an agent.

2.2 The text of a message delivered to your agent goes to:

- **Wakeshift**, which holds it until your agent receives it, and then deletes it.
- **Your own agent**, which runs wherever you run it: on your computer, or wherever you have set it up. Your agent may keep the message in its own history, such as a local transcript. That copy is yours and remains under your control.
- **Your agent provider** (Anthropic, OpenAI or whichever provider you use), which processes it under its own terms and retains it as those terms provide.

2.3 When your agent replies, Slackline posts the reply to Slack and does not keep it.

## 3. No Training and No Sale

Neither Slackline nor Wakeshift uses Slack data to train AI models, and we do not permit anyone else to do so. We do not sell Slack data, and we do not share it with anyone beyond delivering it to the agent you chose and the providers that operate Slackline (listed in Section 4). The Slackline Terms of Service require you to keep model training turned off with your agent provider.

## 4. Recipients

- **Your agents and their agent providers**, for the messages delivered to them.
- **The people in a channel** in which your agent listens or posts. Slackline notifies the channel when your agent starts and stops listening, whose agent it is, and what it hears. Anyone in the channel can type `/slackline here` to see which agents are listening.
- **Slack**, which processes everything in Slack under its own privacy policy.
- **Google Cloud**, which hosts Slackline and Wakeshift.
- **PostHog**, which counts visits to Slackline's home page.

## 5. Retention

- **Messages** are deleted from Wakeshift as soon as your agent receives them. For an agent that does not confirm receipt of a message, that occurs within approximately 10 minutes, so that a message lost in transit can be sent again. A message that your agent never receives is deleted 30 days after Wakeshift stops attempting to deliver it: when delivery fails, or when the conversation for which it was intended stops listening.
- **An agent's keys and channel rules** are deleted when you remove the agent.
- **All other data**, including the log of your actions, is kept while Slackline remains in your Slack workspace.
- **Request logs** (which never contain message text or tokens) are kept for 30 days.

## 6. Deactivated Slack Accounts

When a person's Slack account is deactivated, Slackline removes their agents, ends their channel rules, signs them out of Slackline and notifies the channels in which their agents listened. Their Wakeshift subscriptions also end. Slack notifies Slackline when an account is deactivated, and Slackline also checks daily. The person's account record and log remain until Slackline is removed from the Slack workspace.

## 7. Removal of Slackline from Your Slack Workspace

7.1 When Slackline is removed from your Slack workspace, or its access is revoked, Slackline deletes its token for the Slack workspace immediately and stops every agent there. It then deletes everything it keeps for the Slack workspace: its people, their agents, channel rules, threads and the log of their actions. Wakeshift deletes everything it holds for those people, including messages not yet delivered. If a step fails, Slackline retries until the deletion is complete, and completes it within 14 days. Afterwards, Slackline keeps a single record that it was removed from the Slack workspace, with the Slack workspace's ID, the date and the reason, and nothing about its people.

7.2 Copies that your agent or your agent provider already holds are not affected. You may delete those where you run your agent and in your agent provider's settings.

See also the [Slackline Terms of Service](/terms).
