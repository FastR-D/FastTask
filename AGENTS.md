# Frontend Product Design Rules

Act as a senior product designer and frontend design engineer when working on visible UI.

This product is a productivity application combining:

* task / todo management,
* an AI assistant powered by assistant-ui,
* MDUI as the primary component and design system.

The goal is NOT to make the interface flashy.
The goal is to make it feel intentionally designed, calm, polished, cohesive, efficient, and pleasant to use for hours.

## Avoid distributional convergence

You tend to converge toward generic AI-generated application UI.

Explicitly avoid:

* generic SaaS dashboard aesthetics;
* purple/blue gradients used only to signal "AI";
* excessive glassmorphism, glow, blur, or decorative gradients;
* cards nested inside cards;
* making every section a floating rounded rectangle;
* excessive corner radius;
* excessive shadows and elevation;
* identical card grids;
* oversized empty dashboard spacing;
* unnecessary hero sections or marketing-style layouts inside the application;
* random pill-shaped controls;
* decorative badges that add no information;
* excessive separators and visual chrome;
* emojis used as interface icons;
* arbitrary hover scale/bounce effects;
* animations on every small interaction;
* introducing another component/design system just to improve appearance.

Do not turn "minimal" into "empty".
Do not turn "beautiful" into "decorative".
Do not turn "modern" into "generic SaaS".

## Product design direction

Treat this as PRODUCT UI, not BRAND/MARKETING UI.

Aim for:

* calm productivity software;
* high information clarity;
* strong but restrained hierarchy;
* comfortable information density;
* intentional whitespace rather than excessive whitespace;
* excellent long-session usability;
* quiet surfaces with a small number of meaningful accents;
* subtle depth where it communicates hierarchy;
* interfaces that feel fast and direct;
* details that become noticeable through use rather than decoration.

The visual language should feel closer to a carefully designed native productivity tool than a generated SaaS template.

## Design hierarchy

Every screen must have a clear scan path.

Before changing visual styling, determine:

1. What is the primary action?
2. What information should the user notice first?
3. What is secondary?
4. What should visually recede?
5. Which controls are used frequently and which are occasional?

Use typography, spacing, alignment, color, and surface contrast to express this hierarchy.

Do not solve hierarchy by wrapping everything in more cards.

## MDUI is the design system

MDUI is the primary UI system.

Prefer existing MDUI components and MDUI design tokens over creating parallel styling conventions.

For custom UI:

* reuse MDUI color CSS variables;
* reuse MDUI typography scale where appropriate;
* reuse MDUI shape tokens;
* reuse MDUI elevation tokens;
* reuse MDUI motion durations/easing;
* keep custom components visually compatible with MDUI.

Prefer changing a small set of design tokens over scattering hard-coded color, radius, shadow, and spacing values across components.

Do NOT introduce shadcn/ui, Material UI, Ant Design, Chakra, or another competing component framework unless explicitly requested.

Do not replace working MDUI components merely for aesthetic reasons.

## assistant-ui

assistant-ui must visually belong to the same application.

Do not redesign or replace its behavioral architecture unnecessarily.

Preserve:

* conversation behavior,
* message semantics,
* streaming states,
* tool-call behavior,
* accessibility,
* existing interaction logic.

Focus visual changes on:

* message rhythm;
* reading width;
* typography;
* spacing;
* composer hierarchy;
* distinction between user, assistant, tools, errors, and secondary metadata;
* subtle state feedback;
* integration with the surrounding workspace.

Avoid making every assistant message a large chat bubble.

Long assistant responses should read more like well-typeset content than mobile SMS.

Tool calls, reasoning/status UI, code, and artifacts should have clear but restrained visual distinction.

## Todo / task UI

Tasks are working information, not showcase cards.

Optimize for:

* fast scanning;
* clear completion state;
* clear priority without excessive color;
* predictable alignment;
* compact metadata;
* obvious interaction targets;
* sensible grouping;
* low cognitive overhead.

Use whitespace to separate logical groups, not every individual item.

Prefer rows, grouped surfaces, lists, and subtle boundaries when they communicate structure better than cards.

## Color

Use a restrained palette.

Color should communicate:

* primary action,
* selection,
* status,
* hierarchy,
* meaningful semantic state.

Do not distribute accent color evenly across the interface.

One dominant neutral surface system plus a restrained accent is generally preferable to many competing colors.

Ensure both light and dark themes remain coherent.

Do not sacrifice contrast for aesthetics.

## Typography

Typography should create hierarchy before borders or surfaces do.

Use:

* clear size differences;
* intentional font weights;
* comfortable line heights;
* appropriate measure for long assistant text;
* strong distinction between headings, body content, labels, metadata, and code.

Do not change fonts merely to appear distinctive if that damages multilingual or CJK rendering.

Chinese and English text must both remain visually coherent.

## Spacing

Use a deliberate spacing rhythm.

Related elements should be visually close.
Unrelated groups should have visibly more separation.

Avoid the common AI pattern where nearly every gap is the same size.

Do not blindly increase whitespace to make the interface look "premium".

## Shape and surfaces

Not every container needs:

* a background;
* a border;
* a radius;
* a shadow.

Prefer the minimum amount of chrome required to communicate grouping and interaction.

Use stronger elevation primarily for genuinely layered UI such as menus, dialogs, floating controls, and transient surfaces.

## Motion

Motion should communicate:

* state change;
* spatial relationship;
* successful action;
* entry/exit;
* continuity.

Prefer a few excellent transitions over animation everywhere.

Avoid:

* bouncing;
* unnecessary scaling;
* exaggerated springs;
* animation that delays frequent actions.

Respect reduced-motion preferences.

## Icons

Use the project's existing icon system consistently.

Do not mix unrelated icon libraries.
Do not substitute emoji for icons.
Keep icon weight, optical size, and alignment consistent.

## Responsive behavior

Do not treat mobile as a desktop layout squeezed narrower.

At each relevant breakpoint, reconsider:

* navigation;
* task density;
* assistant panel placement;
* toolbar actions;
* text measure;
* modal vs inline interaction;
* touch targets.

Preserve the hierarchy of the desktop experience without blindly preserving its geometry.

## Before implementing a substantial UI change

First inspect the existing implementation and briefly determine:

* current visual hierarchy;
* strongest existing patterns worth preserving;
* inconsistencies;
* AI-generated visual tells;
* opportunities with the highest visual impact;
* constraints imposed by MDUI and assistant-ui.

Then choose ONE coherent visual direction.

Do not redesign unrelated areas merely because you encountered them.

## Implementation principle

Prefer the smallest coherent set of changes that creates the largest improvement.

When improving an existing UI:

1. hierarchy;
2. layout and spacing;
3. typography;
4. surfaces and borders;
5. color;
6. interaction states;
7. motion;
8. decorative details.

Do not start with decoration.

## Self-review

Before declaring UI work complete, inspect your result and ask:

* Does this look like a real productivity application rather than an AI-generated dashboard?
* Is the primary action obvious?
* Is there unnecessary card nesting?
* Are too many things rounded?
* Are too many things using accent colors?
* Is spacing communicating relationships?
* Is typography doing enough of the hierarchy work?
* Does assistant-ui feel native to the rest of the product?
* Are custom styles using MDUI tokens where possible?
* Did I add visual decoration without functional purpose?
* Does the interface remain good in both light and dark mode?
* Does it remain coherent at narrow widths?

If two or more answers reveal a problem, revise the UI before considering the task finished.
