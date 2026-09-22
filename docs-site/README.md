# PHPRay documentation

Docs site for PHPRay (always-on request tracing for PHP), built with
[MkDocs Material](https://squidfunk.github.io/mkdocs-material/).

## Run locally

```bash
pip install mkdocs-material
cd docs-site
mkdocs serve
```

Open http://localhost:8000 for the live-reloading preview.

## Build a static site

```bash
mkdocs build
```

The static site is written to `site/` and can be served from any static host
(e.g. behind `https://phpray.dev/docs/`).

## Layout

- `mkdocs.yml` — site config, theme (material, dark/light toggle), nav
- `docs/` — Markdown sources, mirroring the navigation in `mkdocs.yml`

All pages must stay under 150 lines. Do not invent measured overhead numbers:
use the placeholder `{{OVERHEAD_NUMBERS}}` until real figures are filled in.
