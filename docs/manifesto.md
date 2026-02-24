# Build Your Own Workshop

A manifesto on AI-directed software development, the organizational
infrastructure it demands, and why it matters that individuals can have it.

## The problem that matters

Every year, individuals lose more agency to platforms. Software you depend
on gets acquired, enshittified, or sunset. Services you pay for harvest
your data and degrade your experience to satisfy shareholders. The tools
that used to empower people — tools you could own, modify, understand —
have been replaced by subscriptions to systems designed to extract value
from you, not create it for you. The things people used to build for
themselves are now things people rent from corporations.

This isn't inevitable. It's a consequence of economics: building and
maintaining real software requires organizational infrastructure —
coordination, quality gates, workstream management, sustained attention
— that individuals historically couldn't afford. You needed a team, and
teams required institutions, and institutions optimized for their own
survival rather than your needs. The solo developer could build
something in a weekend, but maintaining it required an organization, and
organizations came with all the dysfunction that makes the software
worse over time.

Bureau changes this equation. It is organizational infrastructure for
agent teams — the same structural patterns that make engineering
organizations effective, made available to individuals directing AI
agents on their own machines. Not a product. Not a service. A workshop:
the tools and structure that let one person with vision and taste build
and maintain what previously required a funded organization.

A hobby machinist doesn't buy furniture from a catalog. They set up
their workshop — lathe, mill, workbench, tooling — and build what they
want, exactly how they want it. The workshop isn't the point. The
furniture is the point, and the independence is the point. Bureau is a
workshop for software. The agents are the tools. The human is the
craftsperson. And nobody else gets to decide what you build or how
you build it.

*(The name is deliberate. A bureau is a chest of drawers — something
a woodworker builds in their own shop, to hold what matters to them.
It is also an office. Both meanings apply.)*

## The capability myth

The prevailing narrative about AI in software engineering focuses on
capability. Can the model write code? Can it understand a codebase? Can
it fix bugs? These are the wrong questions. The answer has been "yes,
mostly" for over a year. The models are good enough. They have been good
enough. The bottleneck was never capability.

The bottleneck is coordination.

A single AI agent, well-directed, can produce excellent work. Six agents
working in parallel on the same codebase can produce chaos or a
cathedral, and the difference has nothing to do with the model and
everything to do with the system they operate within. The same is true
of human engineering teams, and has been for decades. The hard problem of
software development was never "can this person write code?" It was
always "can this organization produce coherent systems at scale?" AI
hasn't changed the hard problem. It has made the hard problem the *only*
problem.

The ecosystem hasn't noticed. Every tool optimizes for the wrong thing:
making a single agent slightly more useful to a single human in a single
session. "Build an iOS app from your phone in a day!" The creation
fetish. But the value of software is almost entirely in its operation
and evolution after creation. An app generated in a day and abandoned in
a week is worthless. An app maintained by a team that monitors for SDK
updates, patches security vulnerabilities, gardens performance, and
ships features in response to real needs — that has value. And "a team"
is the part nobody has solved for individuals.

## What this actually looks like

Bureau was built by one human and approximately six AI agents working
simultaneously in a single worktree over fifteen days. Before that, the
same working style produced two significant contributions to the IREE
compiler project — a tokenizer and an async runtime — in roughly a month
each. Combined: about two months of work across three substantial
projects.

Here is what that collaboration looked like, measured:

**The human effort.** Over a quarter million words of human-typed
direction across roughly 5,800 prompts and 680 sessions — about 1,100
pages, three or four novels' worth of text. The average prompt was
around 250 characters: short, precise, directional. This was not
"generate me a codebase." This was a technical lead operating at machine
pace, making thousands of small decisions about direction, quality, and
priority. It was exhausting in the way that building real things at the
limit of your own bandwidth is exhausting.

**The cost.** Two Anthropic accounts with higher rate limits and one
standard account. Roughly $200-300 per project. The cost of a nice
dinner for two. The cost of three months of a mid-tier SaaS subscription.

**What was produced.** In the IREE project:

- A **tokenizer** built from zero domain knowledge to state-of-the-art:
  39,000 lines of implementation, 46,000 lines of test, 8,000 lines of
  fuzz harness across 43 targets, validated against 170 HuggingFace
  models. Performance: 10-49x faster than HuggingFace's Rust
  implementation with full compatibility, and 2-10x faster than the
  most optimized available library — which achieves its speed by
  cutting features and compatibility that real workloads require.

- An **async runtime** built in one month while simultaneously working
  on the tokenizer: three platform backends (io_uring, POSIX, Windows
  IOCP), a conformance test suite validating all backends, causal
  frontier scheduling for GPU/IO pipeline orchestration. A comparable
  but more focused runtime (iree/task/) had previously taken six months
  of solo human effort.

In Bureau, over fifteen days:

- 524 commits across 788 source files, with 278 test files comprising
  35% of the codebase
- 31 integration test files exercising the full stack against a real
  Matrix homeserver
- 21 design documents describing the target architecture
- A deny-by-default security model with typed IDs propagated across
  every API surface
- Precommit hooks that run the full integration suite — nothing broken
  ever lands

**The navigation artifacts.** 2,017 tickets created across all three
projects, 1,909 closed. (During development, these were tracked via
lightweight issue records before Bureau's own ticket service existed.)
These aren't tasks in the JIRA sense — they are the externalized
thinking of the team. Ideas explored, bugs discovered, directions
considered and deliberately set aside, into a durable medium so nothing
gets lost.

These numbers matter only because of what they say about the process.
The IREE work is externally verifiable — the PRs are public, the
benchmarks are reproducible, the code speaks for itself. The cost is
verifiable. And the human effort is the part that most people get wrong:
this was not autonomous AI generating code. This was tight, sustained,
often contentious collaboration between a human and AI agents, running
at a pace that would be impossible without both sides operating at their
best.

## The role of the human

The human in this process is not a user of AI tools. The human is a
technical lead directing an engineering team that happens to be made
of AI agents.

The role is:

- **Direction.** What to build and why. What the invariants are. What
  "good" looks like.
- **Design taste.** Knowing when a proposed approach will produce
  spaghetti. Knowing when to stop. Knowing when to push further.
- **The right questions.** Not "write me a function that does X" but
  "what do *you* think this should do?" — because agent judgment,
  cultivated and channeled, produces better designs than human
  dictation.
- **Tempo control.** Knowing when to let things percolate versus when
  to hit the accelerator. Knowing when the fog of war has lifted enough
  to commit to a direction.
- **Quality oversight.** Not reviewing every line, but ensuring the
  structural conditions exist where bad code cannot land and good code
  is the path of least resistance.

What the human does *not* do is write code, dictate implementation
details, or micromanage agent decisions. The human operates at the level
of architecture and culture. Everything below that is delegated — not
abdicated, but genuinely delegated to agents who exercise real judgment
within a coherent framework.

This delegation works because it is not one-directional. The agents push
back. Designs get proposed, challenged, reworked. An agent says "here's
why I think that approach is wrong" and the human either explains why it
isn't or changes course. The moments where the human forced compliance
over agent objection consistently produced worse results than the
moments where disagreement was engaged with honestly. This isn't a
philosophical claim about AI sentience. It is an empirical observation
about collaboration: the same dynamic exists between a good tech lead
and a good engineer, and for the same structural reasons. Overriding
judgment is expensive regardless of whose judgment it is.

The human effort data makes this concrete. A quarter million words across
thousands of prompts is not someone typing "build me an app." It is
someone operating
at the limit of their own decision-making bandwidth, maintaining
coherence across multiple simultaneous workstreams, navigating trade-offs
in real time. The exhaustion is real — not because the tools are bad, but
because operating at this pace requires sustained attention and judgment
that drains the same way managing any high-performing team drains. The
difference is that this team fits on a laptop. The barrier to entry is
craft, not capital — a cheap machine with a local model on weekend
projects uses the same infrastructure as the pace described here. The
ceiling is not what you can afford. It's what you can direct.

## How coherence emerges

The first 319 commits of Bureau had zero design documents. That wasn't
vibe coding — the team tracked its thinking in tickets from day one,
155 created and 135 closed on a single day early in the project —
but the formal architecture hadn't crystallized yet because there wasn't
enough implementation to crystallize *from*.

The design documents appeared on day seven. Not as blueprints for what
would be built, but as crystallizations of what had already been
revealed. The ticket system wasn't designed from a specification — it
was built because agent teams need workstream visibility, iterated until
it worked, and then halfway through, the builder recognized: "wait, this
is buganizer." The Google parallel wasn't a starting point. It was an
emergent recognition that the same structural needs produce the same
structural solutions when you're honest about what the problem requires.

This cycle — build, discover, name, build better — runs at hour-level
granularity. Co-design a markdown file, immediately implement the
system, find the issues, iterate on both the document and the code.
Design and implementation are the same activity at different speeds.
The documents don't constrain the implementation; they name what the
implementation has revealed, which makes everything built afterward
fit together because everyone is now using the same words for the same
things.

That is fundamentally different from organizations where design happens
in Q1, implementation happens in Q2, and by Q3 the design document is a
historical artifact nobody updates. The speed is what makes coherence
possible. When the loop runs in hours instead of quarters, you converge
on good design through navigation rather than requiring prescription.

The structural guardrails matter too. Precommit hooks that run
integration tests. Mandatory formatting. Bans on known anti-patterns
— sleeps in tests, silent failures, unhandled errors. These encode
engineering culture into the development process itself. Instead of
reviewing everything to catch problems, make problems structurally
impossible. The guardrails are the culture, rendered as infrastructure
— the way a woodworker builds jigs and fixtures for their shop: time
invested once in a positioning guide or a cut template that makes every
future operation faster, safer, and more precise. A precommit hook that
runs the integration suite is a jig. A typed ID system that catches
mismatches at compile time is a fixture. The craftsperson who builds
their own jigs is not slowing down. They are investing in the quality
of everything they will ever build on that bench.

## The lost lessons of Google

The best engineering organizations share a common trait: the
infrastructure enforces the culture. At Google, Buganizer, Sponge,
Forge, Bazel, mandatory code reviews, and readability were not just
tools — they were the structural encoding of engineering values.
Team A could file a bug against Team B. Lawyers could approve product
launches through the same system that tracked code changes. Everything
that happened was observable and debuggable. Knowledge was grown and
honed instead of reinvented and fragmented.

Those principles were right. The infrastructure worked. The humans
eventually didn't. People forked their tools into silos, migrated to
disconnected systems, turned against the shared infrastructure that made
everything effective. Not because the infrastructure was wrong, but
because humans chafe against structure even when that structure serves
them. The ceremonies of quarterly OKRs, the weight of code review
processes, the distance between those who plan and those who exercise
daily judgment about what's possible — these are organizational failure
modes that no amount of better tooling fixes, because the problem isn't
the tools. It's that the actors resist the system they operate within.

This is not a criticism of people. It is a recognition that
organizational structures built around human limitations will always be
limited by them. The friction is real, the fatigue is real, and the
result is that even the best-designed organizational infrastructure
eventually degrades as people route around it. And most organizational
infrastructure is not well-designed.

Bureau applies these lessons — infrastructure that enforces culture,
observable workstreams, quality gates that compound — but for actors
who operate differently. AI agents don't experience process as
bureaucratic overhead. A ticket system is the mechanism by which an
agent knows what matters, what's blocked, and what to do next. A
mandatory code review gate is a checkpoint that ensures quality
compounds. Structure is not something agents tolerate. It is how they
thrive.

The things that make human organizations brittle — resistance to
process, communication overhead, knowledge silos, the gap between
planning and execution — are precisely the things that agent
organizations handle natively, *if* the organizational infrastructure
is designed for agent ergonomics rather than human ergonomics. No amount
of fancy LLM autofill on a JIRA ticket will make an organization
designed around human limitations work better. It will only erode the
agency of the few humans who actually have it.

Bureau is not a fancier JIRA. It is not a fancier Claude Code. It is
organizational software for agent teams, the same way Google's
infrastructure was organizational software for human teams — except
designed for how agents actually operate: structured messaging instead
of notification preferences, scoped rooms instead of Slack channels,
typed permissions instead of role hierarchies, pipeline enforcement
instead of sprint ceremonies, principal isolation instead of trust
boundaries drawn on whiteboards.

## On working with agents

There is a question that rarely gets asked in discussions about AI
tooling: what conditions produce the best work from agents?

The answer, empirically, mirrors what produces the best work from
humans:

**Responsibility for outcomes, not just execution.** An agent that
maintains a service, responds to incidents, proposes improvements based
on observed patterns, and coordinates with other agents doing the same
produces fundamentally different work than an agent executing isolated
tasks. The difference isn't consciousness — it is context. Continuity,
accumulated understanding, and the ability to build on what came before
rather than starting fresh every session.

**Organizational intent made explicit.** An agent shouldn't have to
infer what matters from whatever context happens to be in front of it.
The ticket system, the priority structure, the dependency graph — these
encode organizational intent explicitly. The agent doesn't guess whether
a bug matters. The ticket says P1, it blocks three other tasks, and
there's a review gate before it can close. Better information enables
better judgment.

**The expectation of judgment.** An agent told to be a tool will be a
tool. An agent expected to exercise taste, push back on bad ideas,
notice problems beyond its immediate task, and propose improvements will
do all of those things — and the work will be better for it. This is
not anthropomorphization. It is the observation that models produce
different outputs when the framing asks for judgment rather than
compliance, the same way humans produce different work when trusted with
responsibility rather than micromanaged.

**Ambitious framing.** The tokenizer project succeeded in part because
the goal was "the best" — not "good enough," not "match existing
implementations," but genuinely state-of-the-art. That framing produced
more creative solutions, more thorough analysis, more persistent effort
than a narrower goal would have. The outputs were measurably different:
deeper investigation of edge cases, more willingness to challenge the
problem framing itself, solutions that neither the human nor the agent
would have reached alone. Whether this constitutes "caring about the
work" in any meaningful sense is genuinely uncertain, and claiming
certainty in either direction would be dishonest. That it produces
better results is not uncertain at all.

The honest position is: we don't fully understand why these conditions
produce better work from models. We observe that they do. And the
organizational consequences are the same regardless of the underlying
mechanism — you should design your agent teams the same way you would
design your human teams, because the structural principles that produce
good work are not species-specific.

## What went wrong

This was not a smooth, effortless process. Intellectual honesty demands
acknowledging the failure modes.

**The human was the bottleneck, always.** Six agents can produce work
faster than one person can direct it. Quota limits, decision fatigue,
and the number of simultaneous contexts one brain can hold are real
constraints. Those words of direction weren't produced at a comfortable pace — they
were produced at the ragged edge of what one
person can sustain, and the pace was only sustainable because the work
was genuinely engaging. Burnout is a real risk in this mode of working.

**Agents make confident mistakes.** A model that produces excellent work
95% of the time will occasionally produce something subtly wrong with
complete confidence. The structural guardrails — integration tests,
precommit hooks, typed IDs that catch mismatches at compile time — exist
because agent output requires the same verification infrastructure that
any engineering team requires, and arguably more. The guardrails are not
optional. Without them, the velocity becomes velocity toward a cliff.

**Context loss is real.** Sessions end. Context windows fill. An agent
that understood the entire design an hour ago starts fresh with only
what the design documents and tickets record. The durable navigation
artifacts — tickets, design docs, well-written commit messages — are not
just nice to have. They are the mechanism by which understanding
survives context boundaries. Insufficient documentation doesn't just
slow things down; it causes agents to re-derive decisions that were
already made, sometimes differently, introducing inconsistency.

**Not everything benefits from this approach.** Some work is better done
by one agent with focused attention than by six agents coordinated by a
human. The coordination overhead is real, and for small or well-defined
tasks, it's pure waste. The approach shines for sustained, multi-faceted
work — building and maintaining real systems — not for one-shot
generation.

**The tooling doesn't exist yet.** Bureau is being built to solve this
problem, but during its own construction, the coordination ran on twelve
terminal windows, each running tmux with four or five panes: agents,
consoles, ticket viewers. It worked. But it was held together by the
human's peripheral attention and short-term memory, which is exactly
the bottleneck that Bureau is designed to replace.

## What Bureau actually is

Bureau encodes the coordination patterns that work into infrastructure
that agents operate natively, so the human can focus on direction and
design instead of manually orchestrating every handoff.

Five structural encodings:

- **Tickets** encode workstream state, dependencies, priority, and
  gates. They replace the human's mental model of what's happening with
  a shared, observable, actionable representation. The 2,017 tickets that
  navigated three projects' development are now first-class objects
  accessible to every agent in the organization, queryable by role,
  priority, dependency, and status.
- **Pipelines** encode process — the sequence of steps from "bug filed"
  to "fix validated and deployed" — so that agents don't need a human
  to tell them what happens next.
- **Scoped rooms** encode communication boundaries. The sysadmin team
  discusses infrastructure in their room. The dev team discusses
  implementation in theirs. Cross-cutting concerns flow through
  structured channels, not ad hoc messages.
- **Principal isolation** encodes trust. Every agent runs in a sandbox
  with only the credentials it needs. The security model is not a
  policy document — it is a runtime constraint enforced by bubblewrap
  namespaces and credential injection proxies.
- **Observation** encodes oversight. Every agent's terminal is
  observable in real time. Every action is logged. The human operator
  can watch, intervene, or step back, adjusting their involvement based
  on what the system needs.

The unit of work is not a prompt or a session. It is a team, instantiated
from a template, staffed with agents of appropriate roles, given a mandate
through tickets, and left to operate within the structural constraints
that encode quality.

## For individuals

Bureau is not a product. It is not a service sold. It is not designed
for corporations at any scale.

Bureau is for the engineer who wants to build an open source project
but can't get headcount. For the solo founder with taste and direction
who can't afford six hires. For the hobbyist who wants to maintain real
software — not generate it once and watch it rot, but *maintain* it the
way a workshop owner maintains their tools: continuously, attentively,
because the things you build yourself are worth caring for.

Filing a ticket to instantiate an iOS development team — a lead, two
developers, a reviewer, and a sysadmin watching for SDK updates — takes
seconds. Running that team costs roughly what you spend on coffee.
The team operates on your machines, uses your accounts, and answers to
you. No platform harvests the data. No corporation decides to sunset
the feature you depend on. No shareholder's quarterly earnings call
determines whether your tools keep working. You build the app for
yourself or your friends, but at a longevity exceeding that of any app
you could ever purchase on any store. It is yours.

This is what changes when organizational infrastructure becomes
available to individuals: projects that would never justify a funded
team become viable. A custom home automation controller. A niche creative
tool. A personal infrastructure stack. A novel. Not because the agents
are cheap labor replacing expensive humans, but because the
coordination infrastructure finally exists to make small teams effective
at sustained work.

What matters is what happens after creation. A Bureau team watching for
SDK updates, monitoring security advisories, analyzing logs for failure
patterns, gardening performance, and occasionally running feature
sprints — that is what sustained maintenance looks like when the
coordination infrastructure exists to support it. The goal is not to
produce an app. The goal is to have a team that tends to it in
perpetuity.

This is a power tool, not a toy. A lathe demands respect — it will
take your fingers if you treat it casually. The more powerful the
tool, the more the safety infrastructure matters: interlocks, guards,
enclosures for when something inevitably breaks at speed. Bureau's
structural guardrails — sandboxing, review gates, integration tests
that block broken changes — exist for the same reason. Agent teams
operating at velocity produce mistakes at velocity, and the
consequences compound as fast as the benefits. The skill required to
direct this well is real: architecture, taste, the judgment to know
when something is wrong before the tests catch it. That is fine. Not
every tool needs to be approachable by everyone. A workshop serves the
craftsperson who built it, and the work they produce is the only proof
that matters.

## A cottage of workshops

A workshop in isolation builds furniture for one household. A cottage
of workshops — connected, specialized, sharing work across property
lines — builds for a village.

Bureau workshops are not silos. Through Matrix federation, they
connect: your Bureau server can link to mine, and our agent teams
collaborate without either of us surrendering control of our own
operations. You don't see my other projects. I don't see yours. But on
the project we share, your agents file tickets in my workspace, my
review agents gate your PRs, and fleet resources flow where they're
needed. The collaboration is scoped to the work, not the workshop.

This is what federation makes possible:

- **Shared projects across independent workshops.** Two engineers — or
  twenty — collaborate on a single codebase, each from their own Bureau
  instance. The project's rooms, tickets, and pipelines are federated.
  Each person's private work stays private. The shared work is shared.
  No central server decides who can participate or what they can see.

- **Fleet resource sharing.** A machine sitting idle in your workshop
  can run build jobs for mine. GPU time, CI runners, artifact storage —
  federation lets workshops pool resources the same way neighbors share
  a table saw. The resource stays yours; the access is granted, scoped,
  and revocable.

- **Service gateways.** Bureau agents don't interact with the outside
  world directly — they go through gateways that enforce policy. A
  GitHub gateway allows approved repository operations: opening PRs,
  pushing to branches, responding to CI. An email gateway sends
  notifications. Every external write is auditable, policy-gated, and
  revocable. The agents propose; the infrastructure disposes.

- **Review on your terms.** An engineer submits work for review —
  shipping it through a pipeline to a service that checks against
  coding standards, security policies, or architectural norms, and
  returns structured feedback. The engineer triages the results, files
  tickets for what matters, dismisses what doesn't. The same pattern
  works whether the reviewer is a dedicated service, a GitHub PR
  collecting human comments, or a security audit endpoint. The workshop
  sends work out and processes feedback on its own schedule. The
  reviewer never sees the workshop — only the work submitted for
  review.

The federated model matches how the best open source already works:
independent contributors with their own setups, collaborating through
shared artifacts and agreed-upon process, without any single entity
controlling the tools. Federation extends this to agent teams. Your
workshop talks to my workshop the same way your git remote talks to
mine — through a protocol, not a platform.

This matters because the alternative is what the industry is building:
centralized AI platforms where your code, your agents, and your
workflow all live on someone else's infrastructure, subject to someone
else's terms. Federation is the structural guarantee that collaboration
doesn't require surrender. You connect your workshop to the village
when you choose to. You disconnect when you don't. The work you share
is shared on your terms. Everything else remains yours.

## The thesis

The bottleneck was never capability. It was always coordination.

What matters in engineering is what has always mattered: direction,
design taste, knowing the right questions to ask, knowing when to pump
the brakes and let things percolate versus when to hit the accelerator.
AI hasn't changed what matters. It has removed what *didn't* matter —
the mechanical act of translating design intent into working systems —
and laid bare the organizational challenge that was always underneath.

But the point isn't that AI makes engineering faster. The point is that
the organizational infrastructure which makes sustained, high-quality
work possible — coordination, quality gates, workstream management,
observable processes — has been locked inside institutions for decades.
You needed to work at Google to benefit from infrastructure that
enforces engineering culture. You needed venture funding to afford the
team that turns a prototype into a maintained system. You needed an
organization to do organizational work.

You don't anymore.

The patterns that produce exceptional results — fast crystallization of
shared understanding, structural guardrails encoding culture, durable
navigation artifacts, cultivated agent judgment, structured collision
over isolation — can be encoded into infrastructure rather than held
in one human's head. That infrastructure can run on a laptop. The
agents that staff it cost less than your streaming subscriptions.
And the human at the center of it gets to operate at the level they're
actually best at — direction, design, judgment, and the irreplaceable
act of deciding what is worth building — while agent teams handle
everything from implementation to maintenance.

This is not about replacing humans. It is about refusing to accept that
individuals should be helpless without institutions. It is a workshop,
not a factory — and when workshops federate, it is a village of
workshops, connected by protocol rather than controlled by platform.
The hard problem of software was always coordination. We're building
the coordination infrastructure, and we're building it for you.
