---
title: Branding
explainer: devvm's logo, icons, colors and fonts
layout: branding
description: "devvm's logo, icons, social card, colors and fonts, to download and use when you write about devvm."
sidebar:
  exclude: true
# Mostly shortcodes, so no Markdown copy, no Copy page menu and no line in llms.txt.
outputs: [html]
contextMenu: false
llms: false
---

{{< hextra/hero-headline >}}
  Branding
{{< /hextra/hero-headline >}}

{{< hextra/hero-subtitle >}}
  devvm's logo, icons, colors and fonts.
{{< /hextra/hero-subtitle >}}

## The logo

As text, for a terminal, a README or a code comment:

```text
╭──────────╮
│ ❯ devvm ╭┤
╰─────────██
```

As SVG or PNG, with a transparent background:

{{< brand-assets >}}
{{< brand-asset title="For dark backgrounds" src="branding/devvm-logo-on-dark.svg" bg="dark" width="250" height="86" alt="The devvm logo in light gray and lime" files="branding/devvm-logo-on-dark.svg branding/devvm-logo-on-dark.png" >}}
{{< brand-asset title="For light backgrounds" src="branding/devvm-logo-on-light.svg" bg="light" width="250" height="86" alt="The devvm logo in black and dark lime" files="branding/devvm-logo-on-light.svg branding/devvm-logo-on-light.png" >}}
{{< /brand-assets >}}

## Icons

The app icon at any size, and the icons the site uses for browser tabs and home screens:

{{< brand-assets >}}
{{< brand-asset title="App icon" src="branding/devvm-icon-128.png" bg="dark" width="128" height="128" alt="The devvm app icon" files="favicon.svg branding/devvm-icon-64.png branding/devvm-icon-128.png branding/devvm-icon-256.png branding/devvm-icon-512.png branding/devvm-icon-1024.png" >}}
{{< brand-asset title="The site's icons" src="apple-touch-icon.png" bg="dark" width="180" height="180" alt="The devvm icon with a black border, as a home screen shows it" files="favicon.ico favicon-16x16.png favicon-32x32.png apple-touch-icon.png android-chrome-192x192.png android-chrome-512x512.png" >}}
{{< /brand-assets >}}

## The social card

The image to show with a link to devvm. Sites and chat apps show each page of devvm.dev with a card of its own in the same style, which has the page's section, its title and a line on what it covers.

{{< brand-assets wide="true" >}}
{{< brand-asset title="Social card, 1200 by 630" src="images/social-card.png" bg="dark" alt="The devvm social card: the logo, One VM for every project, and the tagline" files="images/social-card.png" >}}
{{< /brand-assets >}}

## Colors

Click a swatch to copy its hex.

{{< palette >}}

## Fonts

All three are free under the SIL Open Font License.

<!-- markdownlint-disable MD034 -- the font-sample shortcodes' parameters, not bare URLs -->

{{< font-sample name="Departure Mono" font="pixel" link="https://departuremono.com/" license="fonts/departure-mono/LICENSE" >}}
A pixel font by Helena Zhang, for the logo, headings and buttons. It's sharp at multiples of 11px.
{{< /font-sample >}}

{{< font-sample name="Instrument Sans" font="sans" link="https://fonts.google.com/specimen/Instrument+Sans" >}}
The font for text.
{{< /font-sample >}}

{{< font-sample name="Martian Mono" font="mono" link="https://fonts.google.com/specimen/Martian+Mono" >}}
The font for code, set narrower than its default and without ligatures.
{{< /font-sample >}}

<!-- markdownlint-enable MD034 -->

## Using the logo

- Write the name in lowercase, as devvm, even at the start of a sentence.
- Keep the lock with the box.
- Leave clear space around the logo, at least as wide as the lock.
- Use the version for your background, and don't recolor, stretch or outline it.

## Credits

The site's pixel icons come from [Dinkie Icons](https://github.com/atelier-anchor/dinkie-icons), which is MIT licensed. The site is built with [Hugo](https://gohugo.io) and the [Hextra](https://imfing.github.io/hextra/) theme.
