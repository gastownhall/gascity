#!/usr/bin/env python3
"""Deterministic effort estimator for wasteland items.

Usage: echo "title" | python3 estimate-effort.py
   or: python3 estimate-effort.py "title string"

Output: trivial | small | medium | large | epic

Effort Guide:
  trivial  — Single file, config tweak, text update, remove something
  small    — 1-3 files, focused bug fix, one component, simple feature
  medium   — 4-10 files, new page/endpoint, integration, moderate refactor
  large    — 10+ files, new system/module, cross-cutting, multi-component
  epic     — Multi-week, new product area, architecture change
"""
import sys
import re

EPIC_PATTERNS = [
    r'deploy.*vercel.*domain',
    r'full.*redesign.*architecture',
]

LARGE_PATTERNS = [
    r'groundedsam', r'photo detail.*ml pipeline', r'docker.compose',
    r'extract.*framework', r'extract.*template', r'gateway.*adk',
    r'launch prep', r'products page.*redesign', r'liveDatasystem',
    r'kanban whiteboard', r'whatsapp gateway', r'new.*system',
    r'full.*integration', r'migrate.*from.*to', r'rewrite',
]

MEDIUM_PATTERNS = [
    r'dashboard:.*page', r'catalog page', r'production build',
    r'production hardening', r'secure redis', r'shared-types',
    r'explore section', r'activity feed', r'case studies',
    r'research lab', r'research section', r'floating.*voice',
    r'interactive.*demo', r'rest api', r'eventbus', r'cli gateway',
    r'visual fixes', r'interactive.*product', r'monorepo',
    r'new.*page', r'new.*endpoint', r'new.*component',
]

SMALL_PATTERNS = [
    r'fix.*cors', r'fix.*xml', r'add.*shelf', r'fix.*crop', r'fix.*race',
    r'formatter', r'client for sending', r'webhook endpoint',
    r'phone.*mapping', r'screenshots', r'move.*button',
    r'split.*component', r'connect.*backend', r'remove.*credential',
    r'assets', r'session management', r'add.*column', r'update.*config',
    r'fix.*bug', r'fix.*typo', r'fix.*error', r'add.*field',
]

TRIVIAL_PATTERNS = [
    r'remove mock', r'simplify.*categor', r'^rename', r'update.*readme',
    r'fix.*typo', r'bump.*version', r'delete.*unused',
]


def estimate_effort(title):
    t = title.lower()
    for p in EPIC_PATTERNS:
        if re.search(p, t, re.I):
            return 'epic'
    for p in LARGE_PATTERNS:
        if re.search(p, t, re.I):
            return 'large'
    for p in MEDIUM_PATTERNS:
        if re.search(p, t, re.I):
            return 'medium'
    for p in SMALL_PATTERNS:
        if re.search(p, t, re.I):
            return 'small'
    for p in TRIVIAL_PATTERNS:
        if re.search(p, t, re.I):
            return 'trivial'

    # Fallback heuristics
    if 'security' in t:
        return 'medium'
    words = len(title.split())
    if words <= 5:
        return 'small'
    if words >= 15:
        return 'large'
    return 'medium'


if __name__ == '__main__':
    if len(sys.argv) > 1:
        title = ' '.join(sys.argv[1:])
    else:
        title = sys.stdin.read().strip()
    print(estimate_effort(title))
