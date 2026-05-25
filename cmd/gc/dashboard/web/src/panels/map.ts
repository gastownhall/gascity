import { api, cityScope } from "../api";
import { byId } from "../util/dom";
import { openLogDrawer } from "./crew";

const SPEECH_DURATION_MS = 10000;
const SPEECH_MAX_LENGTH = 100;
const WADDLER_SPEED_PX_PER_SEC = 140;
const DT_CLAMP_SEC = 0.05;
const TILE_SIZE = 40;
const HALF_TILE = TILE_SIZE / 2;

// ── Steampunk Vector Buildings SVGs ──────────────────────────────────────────

const BuildingBarn = `
<svg viewBox="0 0 160 160" width="160" height="160" overflow="visible">
  <rect x="20" y="60" width="40" height="90" fill="#5D4037" stroke="#3E2723" stroke-width="3"/>
  <path d="M 20 60 C 20 30, 60 30, 60 60" fill="#8D6E63" stroke="#3E2723" stroke-width="3"/>
  <rect x="50" y="80" width="100" height="70" fill="#C62828" stroke="#4A0000" stroke-width="3"/>
  <polygon points="40,80 100,30 160,80" fill="#4E342E" stroke="#3E2723" stroke-width="3"/>
  <circle cx="100" cy="60" r="16" fill="#DAA520" stroke="#B8860B" stroke-width="3" stroke-dasharray="6 4"/>
  <path d="M 80 150 L 80 110 A 20 20 0 0 1 120 110 L 120 150 Z" fill="#3E2723" stroke="#111" stroke-width="3"/>
  <line x1="100" y1="110" x2="100" y2="150" stroke="#111" stroke-width="2"/>
</svg>`;

const BuildingLibrary = `
<svg viewBox="0 0 180 160" width="180" height="160" overflow="visible">
  <rect x="20" y="60" width="140" height="90" fill="#E0E0E0" stroke="#9E9E9E" stroke-width="3"/>
  <rect x="30" y="60" width="15" height="90" fill="#F5F5F5" stroke="#BDBDBD" stroke-width="2"/>
  <rect x="65" y="60" width="15" height="90" fill="#F5F5F5" stroke="#BDBDBD" stroke-width="2"/>
  <rect x="100" y="60" width="15" height="90" fill="#F5F5F5" stroke="#BDBDBD" stroke-width="2"/>
  <rect x="135" y="60" width="15" height="90" fill="#F5F5F5" stroke="#BDBDBD" stroke-width="2"/>
  <path d="M 10 60 Q 90 -10 170 60 Z" fill="#1565C0" stroke="#0D47A1" stroke-width="3"/>
  <polygon points="80,20 100,0 110,5 90,25" fill="#DAA520" stroke="#B8860B" stroke-width="2"/>
  <rect x="70" y="110" width="40" height="40" fill="#3E2723" stroke="#111" stroke-width="3"/>
  <circle cx="90" cy="80" r="12" fill="#4FC3F7" stroke="#0277BD" stroke-width="3" class="anim-flicker"/>
</svg>`;

const BuildingForge = `
<svg viewBox="0 0 160 160" width="160" height="160" overflow="visible">
  <polygon points="10,150 30,70 130,70 150,150" fill="#757575" stroke="#424242" stroke-width="3"/>
  <rect x="100" y="20" width="20" height="50" fill="#9E9E9E" stroke="#616161" stroke-width="3"/>
  <circle cx="110" cy="25" r="12" fill="#BDBDBD" class="anim-smoke"/>
  <path d="M 50 150 L 50 100 C 50 85 110 85 110 100 L 110 150 Z" fill="#212121" stroke="#111" stroke-width="3"/>
  <path d="M 60 150 Q 75 110 80 135 Q 90 110 100 150 Z" fill="#FF5722" class="anim-flicker"/>
  <path d="M 65 150 Q 75 125 80 145 Q 85 125 95 150 Z" fill="#FFC107" class="anim-flicker"/>
</svg>`;

const BuildingFactory = `
<svg viewBox="0 0 160 160" width="160" height="160" overflow="visible">
  <polygon points="10,100 40,70 40,100 70,70 70,100 100,70 100,100 150,100 150,70 150,100" fill="#B0BEC5" stroke="#78909C" stroke-width="3"/>
  <rect x="110" y="30" width="15" height="70" fill="#757575" stroke="#424242" stroke-width="3"/>
  <rect x="135" y="20" width="15" height="80" fill="#757575" stroke="#424242" stroke-width="3"/>
  <circle cx="117" cy="30" r="10" fill="#BDBDBD" class="anim-smoke"/>
  <circle cx="142" cy="20" r="14" fill="#BDBDBD" class="anim-smoke"/>
  <rect x="10" y="100" width="140" height="50" fill="#D84315" stroke="#BF360C" stroke-width="3"/>
  <circle cx="120" cy="125" r="18" fill="#78909C" stroke="#455A64" stroke-width="3" stroke-dasharray="6 4" class="anim-flicker"/>
  <rect x="30" y="110" width="30" height="40" fill="#3E2723" stroke="#111" stroke-width="3"/>
</svg>`;

const BuildingHall = `
<svg viewBox="0 0 180 160" width="180" height="160" overflow="visible">
  <rect x="20" y="70" width="140" height="80" fill="#FFF3E0" stroke="#BCAAA4" stroke-width="3"/>
  <path d="M 10 70 C 10 10 170 10 170 70 Z" fill="#00838F" stroke="#006064" stroke-width="3"/> 
  <circle cx="90" cy="45" r="14" fill="#FFECB3" stroke="#FFB300" stroke-width="3"/>
  <line x1="90" y1="45" x2="90" y2="35" stroke="#111" stroke-width="2"/>
  <line x1="90" y1="45" x2="98" y2="45" stroke="#111" stroke-width="2"/>
  <path d="M 70 150 L 70 110 A 12 12 0 0 1 110 110 L 110 150 Z" fill="#5D4037" stroke="#3E2723" stroke-width="3"/>
</svg>`;

const BuildingTower = `
<svg viewBox="0 0 160 160" width="160" height="160" overflow="visible">
  <polygon points="40,150 120,150 100,50 60,50" fill="#78909C" stroke="#455A64" stroke-width="3"/>
  <rect x="50" y="20" width="60" height="30" fill="#546E7A" stroke="#263238" stroke-width="3"/>
  <circle cx="80" cy="35" r="12" fill="#81D4FA" stroke="#0277BD" stroke-width="3" class="anim-flicker"/>
  <polygon points="40,20 120,20 80,-20" fill="#3949AB" stroke="#1C2833" stroke-width="3"/>
  <path d="M 65 150 L 65 120 A 15 15 0 0 1 95 120 L 95 150 Z" fill="#3E2723" stroke="#111" stroke-width="3"/>
</svg>`;

const BuildingScriptorium = `
<svg viewBox="0 0 160 160" width="160" height="160" overflow="visible">
  <rect x="20" y="70" width="120" height="80" fill="#D7CCC8" stroke="#8D6E63" stroke-width="3"/>
  <line x1="20" y1="70" x2="140" y2="150" stroke="#5D4037" stroke-width="3"/>
  <line x1="140" y1="70" x2="20" y2="150" stroke="#5D4037" stroke-width="3"/>
  <rect x="70" y="70" width="20" height="80" fill="#5D4037"/>
  <polygon points="10,70 80,20 150,70" fill="#8D6E63" stroke="#4E342E" stroke-width="3"/>
  <path d="M 80 10 Q 100 -20 120 -10 Q 110 10 90 20 Z" fill="#FAFAFA" stroke="#BDBDBD" stroke-width="2"/>
  <line x1="80" y1="10" x2="120" y2="-10" stroke="#9E9E9E" stroke-width="2"/>
  <rect x="65" y="110" width="30" height="40" fill="#4E342E" stroke="#111" stroke-width="3"/>
</svg>`;

const BuildingApothecary = `
<svg viewBox="0 0 160 160" width="160" height="160" overflow="visible">
  <rect x="30" y="60" width="100" height="90" fill="#BCAAA4" stroke="#8D6E63" stroke-width="3"/>
  <polygon points="20,60 80,10 140,60" fill="#5D4037" stroke="#3E2723" stroke-width="3"/>
  <line x1="130" y1="70" x2="160" y2="70" stroke="#111" stroke-width="3"/>
  <rect x="140" y="70" width="20" height="30" fill="#FFF8E1" stroke="#FFC107" stroke-width="2"/>
  <circle cx="150" cy="90" r="5" fill="#E91E63"/>
  <polygon points="148,90 152,90 150,75" fill="#E91E63"/>
  <path d="M 65 150 L 65 110 A 15 15 0 0 1 95 110 L 95 150 Z" fill="#795548" stroke="#111" stroke-width="3"/>
</svg>`;

const BuildingMarket = `
<svg viewBox="0 0 160 160" width="160" height="160" overflow="visible">
  <rect x="10" y="80" width="140" height="70" fill="#FFECB3" stroke="#FFB300" stroke-width="3"/>
  <path d="M 0 80 L 160 80 L 150 40 L 10 40 Z" fill="#F44336" stroke="#B71C1C" stroke-width="2"/>
  <polygon points="10,80 30,80 25,40 10,40" fill="#FFF"/>
  <polygon points="50,80 70,80 65,40 55,40" fill="#FFF"/>
  <polygon points="90,80 110,80 105,40 95,40" fill="#FFF"/>
  <polygon points="130,80 150,80 145,40 135,40" fill="#FFF"/>
  <circle cx="80" cy="20" r="16" fill="#FFCA28" stroke="#FF8F00" stroke-width="2"/>
  <rect x="65" y="110" width="30" height="40" fill="#8D6E63" stroke="#3E2723" stroke-width="3"/>
</svg>`;

// ── SVG Canvas Definitions ──────────────────────────────────────────────────

const SvgDefs = `
<defs>
  <pattern id="grass-pat" x="0" y="0" width="40" height="40" patternUnits="userSpaceOnUse">
    <rect width="40" height="40" fill="#7CB342"/>
    <circle cx="10" cy="10" r="2" fill="#689F38"/>
    <circle cx="30" cy="25" r="1.5" fill="#689F38"/>
  </pattern>
  
  <g id="tile-water">
    <rect width="42" height="42" x="-1" y="-1" fill="#2980B9" rx="8"/>
    <path d="M 5 20 Q 15 10 25 20 T 40 20" fill="none" stroke="#4FC3F7" stroke-width="2" stroke-linecap="round" opacity="0.6"/>
  </g>

  <g id="tile-pond">
    <rect width="44" height="44" x="-2" y="-2" fill="#1565C0" rx="14"/>
    <ellipse cx="20" cy="20" rx="12" ry="6" fill="none" stroke="#64B5F6" stroke-width="1.5" opacity="0.5"/>
    <circle cx="8" cy="8" r="4" fill="#4CAF50"/>
    <path d="M 8 8 L 12 8" stroke="#1565C0" stroke-width="1.5"/>
  </g>

  <g id="tile-path">
    <rect width="44" height="44" x="-2" y="-2" fill="#7CB342" rx="8"/>
    <circle cx="8" cy="10" r="1.6" fill="#689F38" opacity="0.8"/>
    <circle cx="32" cy="8" r="1.2" fill="#689F38" opacity="0.7"/>
    <circle cx="34" cy="30" r="1.5" fill="#689F38" opacity="0.75"/>
    <path d="M 4 6 Q 14 0 22 4 Q 30 8 36 6 Q 42 6 44 14 Q 40 22 42 28 Q 44 36 36 38 Q 28 40 22 38 Q 14 36 6 40 Q 0 42 -2 34 Q 2 26 2 22 Q 2 14 4 6 Z" fill="#9A7B54"/>
    <path d="M 6 8 Q 15 3 22 6 Q 30 10 35 9 Q 40 9 42 15 Q 38 22 40 28 Q 42 34 35 35 Q 28 36 22 35 Q 14 33 7 36 Q 2 38 0 32 Q 3 25 3 22 Q 3 14 6 8 Z" fill="#8D6E63" opacity="0.55"/>
    <ellipse cx="16" cy="18" rx="2.6" ry="4" fill="#6D4C41" opacity="0.25" transform="rotate(-18 16 18)"/>
    <ellipse cx="24" cy="26" rx="2.4" ry="3.8" fill="#6D4C41" opacity="0.22" transform="rotate(12 24 26)"/>
    <ellipse cx="30" cy="20" rx="2.2" ry="3.4" fill="#6D4C41" opacity="0.20" transform="rotate(28 30 20)"/>
    <path d="M 6 6 Q 7 3 8 6 M 8 6 Q 9 4 10 6" stroke="#2E7D32" stroke-width="2" fill="none" stroke-linecap="round" opacity="0.9"/>
    <path d="M 36 36 Q 37 33 38 36 M 38 36 Q 39 34 40 36" stroke="#33691E" stroke-width="2" fill="none" stroke-linecap="round" opacity="0.9"/>
  </g>

  <g id="tile-cobble">
    <rect width="44" height="44" x="-2" y="-2" fill="#8D7B6B" rx="4"/>
    <rect width="18" height="18" x="2" y="2" fill="#A89279" rx="4"/>
    <rect width="18" height="18" x="20" y="2" fill="#C4AD8F" rx="4"/>
    <rect width="18" height="18" x="2" y="20" fill="#C4AD8F" rx="4"/>
    <rect width="18" height="18" x="20" y="20" fill="#A89279" rx="4"/>
  </g>

  <g id="tile-plaza">
    <rect width="44" height="44" x="-2" y="-2" fill="#616161"/>
    <polygon points="20,0 40,20 20,40 0,20" fill="#9E9E9E"/>
    <circle cx="20" cy="20" r="8" fill="#BDBDBD" stroke="#424242" stroke-width="2"/>
  </g>

  <g id="tile-stone-wall">
    <rect width="42" height="42" x="-1" y="-1" fill="#6D5C4F"/>
    <path d="M -1 1 H 41" stroke="#BFAF9F" stroke-width="2" opacity="0.65"/>
    <path d="M -1 4 H 41" stroke="#5B4B41" stroke-width="2" opacity="0.25"/>
    <rect width="18" height="12" x="0" y="0" fill="#8A7A6C" stroke="#4E4037" stroke-width="1"/>
    <rect width="22" height="12" x="18" y="0" fill="#9B8A7A" stroke="#4E4037" stroke-width="1"/>
    <rect width="24" height="12" x="-4" y="12" fill="#9B8A7A" stroke="#4E4037" stroke-width="1"/>
    <rect width="20" height="12" x="20" y="12" fill="#8A7A6C" stroke="#4E4037" stroke-width="1"/>
    <rect width="18" height="12" x="0" y="24" fill="#8A7A6C" stroke="#4E4037" stroke-width="1"/>
    <rect width="22" height="12" x="18" y="24" fill="#9B8A7A" stroke="#4E4037" stroke-width="1"/>
  </g>

  <g id="tile-bridge">
    <rect width="44" height="44" x="-2" y="-2" fill="#5D4037" rx="4"/>
    <rect width="44" height="6" x="-2" y="5" fill="#3E2723"/>
    <rect width="44" height="6" x="-2" y="29" fill="#3E2723"/>
  </g>

  <g id="tile-farm">
    <rect width="42" height="42" x="-1" y="-1" fill="#795548" rx="4"/>
    <line x1="0" y1="10" x2="40" y2="10" stroke="#5D4037" stroke-width="2"/>
    <line x1="0" y1="20" x2="40" y2="20" stroke="#5D4037" stroke-width="2"/>
    <line x1="0" y1="30" x2="40" y2="30" stroke="#5D4037" stroke-width="2"/>
    <path d="M 5 5 Q 10 0 15 5 M 10 5 L 10 10" stroke="#4CAF50" stroke-width="2" fill="none" stroke-linecap="round"/>
    <path d="M 25 5 Q 30 0 35 5 M 30 5 L 30 10" stroke="#4CAF50" stroke-width="2" fill="none" stroke-linecap="round"/>
    <path d="M 15 15 Q 20 10 25 15 M 20 15 L 20 20" stroke="#8BC34A" stroke-width="2" fill="none" stroke-linecap="round"/>
    <path d="M 5 25 Q 10 20 15 25 M 10 25 L 10 30" stroke="#4CAF50" stroke-width="2" fill="none" stroke-linecap="round"/>
    <path d="M 25 25 Q 30 20 35 25 M 30 25 L 30 30" stroke="#8BC34A" stroke-width="2" fill="none" stroke-linecap="round"/>
  </g>

  <g id="tile-fountain-grand">
    <rect width="88" height="88" x="-4" y="-4" fill="#E0E0E0" rx="8"/>
    <circle cx="40" cy="40" r="36" fill="#BDBDBD" stroke="#757575" stroke-width="3"/>
    <circle cx="40" cy="40" r="30" fill="#2980B9"/>
    <circle cx="40" cy="40" r="16" fill="#9E9E9E" stroke="#616161" stroke-width="2"/>
    <circle cx="40" cy="40" r="12" fill="#3498DB"/>
    <circle cx="40" cy="40" r="6" fill="#757575"/>
    <path d="M 40 40 Q 30 10 15 35 M 40 40 Q 50 10 65 35 M 40 40 L 40 5" stroke="#81D4FA" stroke-width="2" fill="none" class="anim-fountain" stroke-linecap="round"/>
    <path d="M 20 40 Q 25 30 30 40 M 60 40 Q 55 30 50 40 M 40 20 Q 30 25 40 30 M 40 60 Q 50 55 40 50" stroke="#B3E5FC" stroke-width="1.5" fill="none" class="anim-fountain" style="animation-delay: 0.5s"/>
    <circle cx="40" cy="40" r="22" fill="none" stroke="#64B5F6" stroke-width="1.5" opacity="0.5" class="anim-ripple"/>
    <circle cx="40" cy="40" r="28" fill="none" stroke="#64B5F6" stroke-width="1" opacity="0.3" class="anim-ripple-delay"/>
  </g>

  <g id="tree-pine">
    <polygon points="20,0 40,30 30,30 45,55 5,55 20,30 10,30" fill="#2E7D32"/>
    <polygon points="20,0 40,30 30,30 45,55 20,55" fill="#1B5E20" opacity="0.3"/>
    <rect x="16" y="55" width="8" height="15" fill="#4E342E"/>
  </g>

  <g id="tree-oak">
    <circle cx="20" cy="20" r="22" fill="#558B2F"/>
    <circle cx="10" cy="15" r="14" fill="#689F38"/>
    <circle cx="30" cy="25" r="12" fill="#33691E"/>
    <rect x="16" y="38" width="8" height="20" fill="#5D4037"/>
  </g>

  <g id="bush">
    <circle cx="20" cy="25" r="12" fill="#43A047"/>
    <circle cx="12" cy="30" r="8" fill="#2E7D32"/>
    <circle cx="28" cy="30" r="8" fill="#2E7D32"/>
    <circle cx="15" cy="20" r="3" fill="#F48FB1"/>
    <circle cx="25" cy="18" r="3" fill="#CE93D8"/>
  </g>

  <g id="rock">
    <path d="M 10 35 Q 15 20 25 20 T 35 35 Z" fill="#9E9E9E"/>
    <path d="M 15 35 Q 20 25 25 25 T 30 35 Z" fill="#BDBDBD"/>
  </g>
  
  <g id="deco-lamp">
    <rect x="18" y="10" width="4" height="30" fill="#212121"/>
    <polygon points="15,40 25,40 22,10 18,10" fill="#424242"/>
    <rect x="15" y="-5" width="10" height="15" fill="none" stroke="#212121" stroke-width="2"/>
    <circle cx="20" cy="2" r="5" fill="#FFCA28" stroke="#FFF59D" stroke-width="2" class="anim-flicker"/>
    <polygon points="12,-5 28,-5 20,-15" fill="#212121"/>
  </g>

  <g id="deco-pipes">
    <circle cx="22" cy="-6" r="6" fill="#BDBDBD" opacity="0.85" class="anim-smoke" style="animation-delay: -0.2s"/>
    <circle cx="14" cy="-10" r="4.5" fill="#E0E0E0" opacity="0.75" class="anim-smoke" style="animation-delay: -0.9s"/>
    <circle cx="26" cy="-13" r="3.5" fill="#EEEEEE" opacity="0.65" class="anim-smoke" style="animation-delay: -1.5s"/>
    <ellipse cx="20" cy="38" rx="14" ry="4" fill="#33691E" opacity="0.18"/>
    <rect x="8" y="22" width="24" height="18" rx="2" fill="#A24A3A" stroke="#5D2B22" stroke-width="2"/>
    <path d="M 10 28 H 30 M 10 34 H 30" stroke="#7A3328" stroke-width="2" opacity="0.8"/>
    <path d="M 14 22 V 40 M 22 22 V 40 M 30 22 V 40" stroke="#D7CCC8" stroke-width="1" opacity="0.35"/>
    <path d="M 14 24 L 26 24 L 24 4 L 16 4 Z" fill="#7D3A2C" stroke="#3E2723" stroke-width="2"/>
    <path d="M 15 6 L 25 6" stroke="#B87333" stroke-width="2" stroke-linecap="round" opacity="0.75"/>
    <path d="M 18 6 L 19 23" stroke="#BCAAA4" stroke-width="2" stroke-linecap="round" opacity="0.35"/>
  </g>

  <g id="deco-gear">
    <ellipse cx="20" cy="38" rx="16" ry="4.5" fill="#33691E" opacity="0.16"/>
    <g transform="translate(2,16) skewX(-8)">
      <rect x="0" y="6" width="18" height="16" rx="2" fill="#8D6E63" stroke="#3E2723" stroke-width="2"/>
      <path d="M 2 10 H 16 M 2 14 H 16 M 2 18 H 16" stroke="#5D4037" stroke-width="1.5" opacity="0.55"/>
      <path d="M 2 6 L 16 22 M 16 6 L 2 22" stroke="#3E2723" stroke-width="2" opacity="0.65"/>
    </g>
    <g transform="translate(18,10)">
      <ellipse cx="10" cy="10" rx="8" ry="4" fill="#A67C52" stroke="#3E2723" stroke-width="2"/>
      <rect x="2" y="10" width="16" height="20" rx="7" fill="#9C6B3C" stroke="#3E2723" stroke-width="2"/>
      <ellipse cx="10" cy="30" rx="8" ry="4" fill="#8D5E34" stroke="#3E2723" stroke-width="2"/>
      <path d="M 6 12 V 28 M 10 12 V 28 M 14 12 V 28" stroke="#6D4C41" stroke-width="2" opacity="0.55" stroke-linecap="round"/>
      <path d="M 3 17 H 17" stroke="#B87333" stroke-width="2.5" opacity="0.95" stroke-linecap="round"/>
      <path d="M 3 24 H 17" stroke="#DAA520" stroke-width="2.5" opacity="0.95" stroke-linecap="round"/>
    </g>
    <g transform="translate(10,14) scale(0.85)">
      <ellipse cx="10" cy="10" rx="8" ry="4" fill="#B07D4A" stroke="#3E2723" stroke-width="2"/>
      <rect x="2" y="10" width="16" height="20" rx="7" fill="#A06B3B" stroke="#3E2723" stroke-width="2"/>
      <ellipse cx="10" cy="30" rx="8" ry="4" fill="#8D5E34" stroke="#3E2723" stroke-width="2"/>
      <path d="M 4 20 H 16" stroke="#B87333" stroke-width="2.5" opacity="0.9" stroke-linecap="round"/>
    </g>
    <path d="M 10 30 C 13 26 18 26 21 30" fill="none" stroke="#BCAAA4" stroke-width="2" stroke-linecap="round" opacity="0.9"/>
    <circle cx="21" cy="30" r="2" fill="#BCAAA4" opacity="0.9"/>
  </g>

  <!-- Ground-walking Goose Character -->
  <g id="goose">
    <polygon points="16,40 4,32 8,46" fill="#FAFAFA" stroke="#B0BEC5" stroke-width="2" stroke-linejoin="round"/>
    <path d="M 38,36 Q 42,24 38,14 L 28,16 Q 32,26 30,36 Z" fill="#FAFAFA"/>
    <path d="M 38,36 Q 42,24 38,14 M 28,16 Q 32,26 30,36" fill="none" stroke="#B0BEC5" stroke-width="2"/>
    <ellipse cx="30" cy="40" rx="16" ry="12" fill="#FAFAFA" stroke="#B0BEC5" stroke-width="2"/>
    <circle cx="34" cy="14" r="8" fill="#FAFAFA" stroke="#B0BEC5" stroke-width="2"/>
    <path d="M 30,36 Q 34,32 38,36 M 28,16 Q 32,18 38,14" fill="#FAFAFA" stroke="none"/>
    <circle cx="36" cy="12" r="4" fill="#263238"/>
    <circle cx="37" cy="11" r="1.5" fill="#81D4FA"/>
    <path d="M 32 12 L 24 14" stroke="#5D4037" stroke-width="2" fill="none" stroke-linecap="round"/>
    <polygon points="41,12 49,14 43,18" fill="#FF9800" stroke="#E65100" stroke-width="1.5" stroke-linejoin="round"/>
    <rect x="22" y="51" width="4" height="8" fill="#FF9800"/>
    <polygon points="22,59 30,59 27,62" fill="#E65100"/>
    <rect x="32" y="51" width="4" height="8" fill="#FF9800"/>
    <polygon points="32,59 40,59 37,62" fill="#E65100"/>
  </g>

  <!-- Top Hat Accessory for Mayor/Supervisor -->
  <g id="top-hat">
    <line x1="24" y1="6" x2="44" y2="6" stroke="#212121" stroke-width="4" stroke-linecap="round"/>
    <rect x="28" y="-12" width="12" height="18" fill="#212121"/>
    <rect x="28" y="2" width="12" height="3" fill="#FFC107"/>
  </g>

  <!-- Swimming Goose character -->
  <g id="goose-swimming">
    <ellipse cx="20" cy="20" rx="15" ry="5" fill="none" stroke="#64B5F6" stroke-width="2" class="anim-ripple"/>
    <ellipse cx="20" cy="20" rx="25" ry="8" fill="none" stroke="#64B5F6" stroke-width="1.5" class="anim-ripple-delay"/>
    <g class="anim-swim-bob" transform="translate(-10, -20)">
      <polygon points="16,40 4,32 8,46" fill="#FAFAFA" stroke="#B0BEC5" stroke-width="2" stroke-linejoin="round"/>
      <path d="M 38,36 Q 42,24 38,14 L 28,16 Q 32,26 30,36 Z" fill="#FAFAFA"/>
      <path d="M 38,36 Q 42,24 38,14 M 28,16 Q 32,26 30,36" fill="none" stroke="#B0BEC5" stroke-width="2"/>
      <path d="M 14,40 A 16 12 0 0 1 46 40 Z" fill="#FAFAFA" stroke="#B0BEC5" stroke-width="2"/>
      <circle cx="34" cy="14" r="8" fill="#FAFAFA" stroke="#B0BEC5" stroke-width="2"/>
      <path d="M 30,36 Q 34,32 38,36 M 28,16 Q 32,18 38,14" fill="#FAFAFA" stroke="none"/>
      <circle cx="36" cy="12" r="4" fill="#263238"/>
      <circle cx="37" cy="11" r="1.5" fill="#81D4FA"/>
      <path d="M 32 12 L 24 14" stroke="#5D4037" stroke-width="2" fill="none" stroke-linecap="round"/>
      <polygon points="41,12 49,14 43,18" fill="#FF9800" stroke="#E65100" stroke-width="1.5" stroke-linejoin="round"/>
    </g>
  </g>
  <g id="acc-book">
    <rect x="36" y="32" width="22" height="15" rx="2" fill="#D7CCC8" stroke="#5D4037" stroke-width="1.5" />
    <path d="M 47,32 L 47,47" stroke="#5D4037" stroke-width="1" />
    <path d="M 39,36 H 45 M 39,40 H 45 M 49,36 H 55 M 49,40 H 55" stroke="#795548" stroke-width="1" />
  </g>
  <g id="acc-laptop">
    <rect x="36" y="30" width="22" height="15" rx="1.5" fill="#3E2723" stroke="#5D4037" stroke-width="1.5" />
    <rect x="39" y="33" width="16" height="9" fill="#000" />
    <rect x="41" y="35" width="12" height="5" fill="#00E676" opacity="0.8" />
    <path d="M 36,45 L 34,48 H 60 L 58,45 Z" fill="#5D4037" stroke="#3E2723" stroke-width="1" />
  </g>
  <g id="acc-wrench" class="anim-spin" style="transform-origin: 47px 24px;">
    <path d="M 44,14 L 46,24 L 50,24 L 48,14" fill="#B0BEC5" stroke="#37474F" stroke-width="1" />
    <circle cx="44" cy="14" r="4.5" fill="#B0BEC5" stroke="#37474F" stroke-width="1.5" />
    <polygon points="41,12 45,12 43,15" fill="#37474F" />
    <circle cx="48" cy="24" r="2.5" fill="#78909C" />
  </g>
  <g id="acc-question-mark" class="v-status-alert">
    <circle cx="48" cy="18" r="9" fill="#FF9800" stroke="#E65100" stroke-width="1.5" />
    <text x="48" y="21" font-family="monospace" font-size="11" font-weight="bold" fill="#FFF" text-anchor="middle">?</text>
  </g>
</defs>
`;

// ── Canonical Map Data ───────────────────────────────────────────────────────

export const MAP_CONFIG = `
TTT.T.OOOTTT..TT.TTTT.TTTT.TT.TTTT.O~~~.TTTT..TT..TTTT.TT..T
TTT*TTT.T.T...TT.T.TTT.TTTTTTO.....~~~..TTT.T.TT.O.TTTTO.T.
..T............*..T..O..............~~~..................T.T
.O........O.T....O..R...........T.O.~~~.....................
TT..................................~~~.O.................TT
T...:::::l.....T....T..l::::..O.....~~~...................*.
..T.::L:::::::::::::::::::H:::::::::===::########.....l.....
....:::::.*O.T.T....O.....:....O..l:~~~:%%%%%%%######I....T.
T*..:::::..R...*..l+++++++++++++l..:~~~:::::pp%*..........*.
.....l:############++++++KK+++++...:~~~::F::pp%.......O...TT
######:............+++++++++++++O..:~~~:::::pp%.....O......T
......:............+++++++++++++...:~~~:::::pp%.T.........T.
.*...R:......OT...l+++M+++++++S+l..:~~~:::::pp%*..O...*....T
T.....:.....TOOT..O******l:l****O..:~~~:::::pp%.....*.......
.O....:......OO.....R.....:........:~~~:::::pp%.........O...
T..R..:.............T....*:*....O..:~~~:::::pp%...........TT
......:l::W::..l...l*..l..:.l.....l:~~~:::::pp%...*.......OT
.T....::::::::::::::::::::::::::::::===:::::pp%.....O..*....
..O......#..T.......T.#.:.......O..O~~~:::::pp%*..........T.
T.......#.O.T..R....*.#l:AAAAAAAAAAA~~~:::::pp%....R...*....
.*....OPPP..O.......AAAA:AAAAAAAAAAA~~~::C::pp%..O..........
T.....PPPPP.O..*....AAAA:##AAAAAAAAA~~~%%%%%%%%*..........TT
..O..TPPPPPP........AAAA::BAAAAAAAAA~~~....................T
.T*OOOPPPPPPTTTT.TTTAAAAAAAAAAAAAAAA~~~T.TTT.TTT.TT.T.TT..T.
T.TOOTT.T..T.T.TTTT.T*TT.TT.O.T.T.T.~~~TT...TT.TTT..T..T.TOT
`;

export interface MapBuilding {
  key: string;
  label: string;
  svg: string;
  gridX: number;
  gridY: number;
}

const BUILDINGS: Record<string, { key: string; label: string; svg: string }> = {
  H: { key: "hall", label: "City Hall (session)", svg: BuildingHall },
  L: { key: "library", label: "Formulas Lab (formulas)", svg: BuildingLibrary },
  B: { key: "barn", label: "Watchdog Outpost (patrol)", svg: BuildingBarn },
  F: { key: "forge", label: "Launcher Pad (sling)", svg: BuildingForge },
  C: { key: "factory", label: "Dolt Vault (beads)", svg: BuildingFactory },
  I: { key: "inspector", label: "Review Post (mail)", svg: BuildingTower },
  W: { key: "scriptorium", label: "Prompt Ink-shop (prompts)", svg: BuildingScriptorium },
  S: { key: "apothecary", label: "Security Citadel (config)", svg: BuildingApothecary },
  M: { key: "market", label: "Radio Tower (events)", svg: BuildingMarket },
};

const BUILDING_CHARS = Object.keys(BUILDINGS);

// Home buildings based on agent template name matchers
const AGENT_TEMPLATES_MAPPING: Array<{ pattern: RegExp; key: string }> = [
  { pattern: /supervisor|mayor/i, key: "hall" },
  { pattern: /antigravity|developer/i, key: "factory" },
  { pattern: /security/i, key: "apothecary" },
  { pattern: /sre|dog/i, key: "library" },
  { pattern: /ci|refinery|sling/i, key: "forge" },
  { pattern: /cd|deacon|prompt/i, key: "scriptorium" },
  { pattern: /review/i, key: "inspector" },
  { pattern: /polecat|watchdog|patrol/i, key: "barn" },
];

function getHomeBuildingKey(template: string): string {
  if (!template) return "market";
  for (const m of AGENT_TEMPLATES_MAPPING) {
    if (m.pattern.test(template)) return m.key;
  }
  return "market";
}

let TOWN_MAP_GRID: string[][] = [];
const BUILDING_POSITIONS: Record<string, MapBuilding> = {};
let MAP_WIDTH = 0;
let MAP_HEIGHT = 0;

function initMapGrid(): void {
  if (TOWN_MAP_GRID.length > 0) return;
  const rawLines = MAP_CONFIG.trim().split("\n").filter((l) => l.trim().length > 0);
  TOWN_MAP_GRID = rawLines.map((line) => line.split(""));
  MAP_WIDTH = (TOWN_MAP_GRID[0]?.length || 0) * TILE_SIZE;
  MAP_HEIGHT = TOWN_MAP_GRID.length * TILE_SIZE;

  for (let y = 0; y < TOWN_MAP_GRID.length; y++) {
    for (let x = 0; x < TOWN_MAP_GRID[y].length; x++) {
      const char = TOWN_MAP_GRID[y][x];
      if (BUILDINGS[char]) {
        const b = BUILDINGS[char];
        BUILDING_POSITIONS[b.key] = {
          ...b,
          gridX: x,
          gridY: y,
        };
      }
    }
  }
}

// ── Pathfinding (A* algorithm) ────────────────────────────────────────────────

interface PathNode {
  x: number;
  y: number;
  g: number;
  f: number;
  parent: PathNode | null;
}

function findPath(startX: number, startY: number, endX: number, endY: number): Array<{ x: number; y: number }> {
  const grid = TOWN_MAP_GRID;
  if (!grid.length || !grid[0].length) return [];
  const w = grid[0].length;
  const h = grid.length;

  const getCost = (nx: number, ny: number): number => {
    if (nx < 0 || ny < 0 || nx >= w || ny >= h) return Infinity;
    const char = grid[ny][nx];
    if (char === ":" || char === "+" || char === "=" || BUILDING_CHARS.includes(char)) {
      return 1;
    }
    if (char === "#") return 2;
    if (char === "." || char === "A" || char === "K") return 5;
    return Infinity;
  };

  const heuristic = (nx: number, ny: number): number => Math.abs(nx - endX) + Math.abs(ny - endY);

  const openSet: PathNode[] = [
    { x: startX, y: startY, g: 0, f: heuristic(startX, startY), parent: null },
  ];
  const closedSet = new Set<string>();
  const hash = (nx: number, ny: number) => `${nx},${ny}`;

  while (openSet.length > 0) {
    openSet.sort((a, b) => a.f - b.f);
    const curr = openSet.shift()!;

    if (curr.x === endX && curr.y === endY) {
      const path: Array<{ x: number; y: number }> = [];
      let node: PathNode | null = curr;
      while (node) {
        if (node.parent) {
          path.unshift({ x: node.x, y: node.y });
        }
        node = node.parent;
      }
      return path;
    }

    closedSet.add(hash(curr.x, curr.y));

    const neighbors = [
      { x: curr.x + 1, y: curr.y },
      { x: curr.x - 1, y: curr.y },
      { x: curr.x, y: curr.y + 1 },
      { x: curr.x, y: curr.y - 1 },
    ];

    for (const n of neighbors) {
      if (closedSet.has(hash(n.x, n.y))) continue;
      const cost = getCost(n.x, n.y);
      if (cost === Infinity) continue;

      const g = curr.g + cost;
      const existing = openSet.find((o) => o.x === n.x && o.y === n.y);

      if (!existing) {
        openSet.push({
          x: n.x,
          y: n.y,
          g,
          f: g + heuristic(n.x, n.y),
          parent: curr,
        });
      } else if (g < existing.g) {
        existing.g = g;
        existing.f = g + heuristic(n.x, n.y);
        existing.parent = curr;
      }
    }
  }
  return [];
}

// ── Tile & Decoration Mapping ────────────────────────────────────────────────

function getTileHtml(char: string, px: number, py: number): string {
  switch (char) {
    case "~":
      return `<use href="#tile-water" x="${px}" y="${py}"/>`;
    case "P":
      return `<use href="#tile-pond" x="${px}" y="${py}"/>`;
    case "#":
      return `<use href="#tile-path" x="${px}" y="${py}"/>`;
    case ":":
      return `<use href="#tile-cobble" x="${px}" y="${py}"/>`;
    case "+":
      return `<use href="#tile-plaza" x="${px}" y="${py}"/>`;
    case "%":
      return `<use href="#tile-stone-wall" x="${px}" y="${py}"/>`;
    case "=":
      return `<use href="#tile-bridge" x="${px}" y="${py}"/>`;
    case "A":
      return `<use href="#tile-farm" x="${px}" y="${py}"/>`;
    default:
      if (BUILDING_CHARS.includes(char)) {
        return `<use href="#tile-plaza" x="${px}" y="${py}"/>`;
      }
      return "";
  }
}

function getDecoHtml(char: string, px: number, py: number): string {
  switch (char) {
    case "T": {
      const seed = (px * 7 + py * 13) & 0xffff;
      const jx = px + ((seed % 10) - 5);
      const jy = py + (((seed >> 4) % 10) - 5) - 20;
      return `<use href="#tree-pine" x="${jx}" y="${jy}"/>`;
    }
    case "O":
      return `<use href="#tree-oak" x="${px}" y="${py - 15}"/>`;
    case "*":
      return `<use href="#bush" x="${px}" y="${py}"/>`;
    case "R":
      return `<use href="#rock" x="${px}" y="${py}"/>`;
    case "l":
      return `<use href="#deco-lamp" x="${px}" y="${py}"/>`;
    case "p":
      return `<use href="#deco-pipes" x="${px}" y="${py}"/>`;
    case "G":
      return `<use href="#deco-gear" x="${px}" y="${py}"/>`;
    default:
      return "";
  }
}

let cachedBackgroundHtml = "";

function generateBackgroundHtml(): string {
  if (cachedBackgroundHtml) return cachedBackgroundHtml;
  initMapGrid();

  const tiles: string[] = [];
  const decorations: Array<{ y: number; html: string }> = [];
  let hasSwimmingGoose = false;
  let hasFarmerGoose = false;

  for (let y = 0; y < TOWN_MAP_GRID.length; y++) {
    for (let x = 0; x < TOWN_MAP_GRID[y].length; x++) {
      const char = TOWN_MAP_GRID[y][x];
      const px = x * TILE_SIZE;
      const py = y * TILE_SIZE;

      const tile = getTileHtml(char, px, py);
      if (tile) tiles.push(tile);

      if (char === "P" && (!hasSwimmingGoose || Math.random() < 0.05)) {
        hasSwimmingGoose = true;
        const delay = -(Math.random() * 5).toFixed(2);
        decorations.push({
          y: py,
          html: `
            <g transform="translate(${px}, ${py})">
              <g class="anim-wander" style="animation-delay: ${delay}s">
                <use href="#goose-swimming"/>
              </g>
            </g>
          `,
        });
      } else if (char === "K") {
        const isTopLeft =
          (x === 0 || TOWN_MAP_GRID[y][x - 1] !== "K") &&
          (y === 0 || TOWN_MAP_GRID[y - 1][x] !== "K");
        if (isTopLeft) {
          decorations.push({
            y: py + TILE_SIZE,
            html: `<use href="#tile-fountain-grand" x="${px}" y="${py}"/>`,
          });
        }
      } else if (char === "A" && !hasFarmerGoose) {
        hasFarmerGoose = true;
        decorations.push({
          y: py + 20,
          html: `
            <g transform="translate(${px}, ${py - 10})">
              <g class="anim-farm-wander">
                <foreignObject x="-32" y="-64" width="64" height="64" style="overflow: visible;">
                  <div class="v-goose-anim walking" style="width: 64px; height: 64px;">
                    <svg viewBox="0 0 64 64" width="100%" height="100%"><use href="#goose"/></svg>
                  </div>
                </foreignObject>
              </g>
            </g>
          `,
        });
      }

      const deco = getDecoHtml(char, px, py);
      if (deco) {
        const decoY = char === "T" ? py - 20 : char === "O" ? py - 15 : py;
        decorations.push({ y: decoY, html: deco });
      }
    }
  }

  // Draw ambient forest around the main grid
  const ambientForest: Array<{ y: number; html: string }> = [];
  for (let y = -400; y < MAP_HEIGHT + 400; y += 40) {
    for (let x = -800; x < MAP_WIDTH + 800; x += 40) {
      if (x >= 0 && x < MAP_WIDTH && y >= 0 && y < MAP_HEIGHT) continue;

      let isRiver = false;
      const gridX = Math.floor(x / TILE_SIZE);
      const gridY = Math.floor(y / TILE_SIZE);

      if (x >= 0 && x < MAP_WIDTH) {
        if (y < 0 && TOWN_MAP_GRID[0] && TOWN_MAP_GRID[0][gridX] === "~") isRiver = true;
        if (
          y >= MAP_HEIGHT &&
          TOWN_MAP_GRID[TOWN_MAP_GRID.length - 1] &&
          TOWN_MAP_GRID[TOWN_MAP_GRID.length - 1][gridX] === "~"
        ) {
          isRiver = true;
        }
      }
      if (y >= 0 && y < MAP_HEIGHT) {
        if (x < 0 && TOWN_MAP_GRID[gridY] && TOWN_MAP_GRID[gridY][0] === "~") isRiver = true;
        if (
          x >= MAP_WIDTH &&
          TOWN_MAP_GRID[gridY] &&
          TOWN_MAP_GRID[gridY][TOWN_MAP_GRID[gridY].length - 1] === "~"
        ) {
          isRiver = true;
        }
      }

      if (isRiver) {
        tiles.push(`<use href="#tile-water" x="${x}" y="${y}"/>`);
        continue;
      }

      const hashSeed = (x * 3 + y * 11) & 0xfff;
      if ((hashSeed % 10) < 4) {
        const type = (hashSeed % 2) === 0 ? "#tree-pine" : "#tree-oak";
        const jx = x + ((hashSeed % 20) - 10);
        const jy = y + (((hashSeed >> 4) % 20) - 10);
        ambientForest.push({
          y: jy,
          html: `<use href="${type}" x="${jx}" y="${jy}"/>`,
        });
      }
    }
  }

  const allDecorations = [...ambientForest, ...decorations];
  allDecorations.sort((a, b) => a.y - b.y);

  cachedBackgroundHtml = `
    ${tiles.join("\n")}
    ${allDecorations.map((d) => d.html).join("\n")}
  `;
  return cachedBackgroundHtml;
}

// ── Living Character State Map ────────────────────────────────────────────────

interface CharacterState {
  id: string;
  template: string;
  x: number;
  y: number;
  offsetX: number;
  offsetY: number;
  targetGridX: number;
  targetGridY: number;
  status: "idle" | "working" | "questions" | "finished";
  subAction: "reading" | "writing" | "executing" | "idle";
  hasPending: boolean;
  action: "idle" | "walking" | "working";
  hidden: boolean;
  speech: string | null;
  speechTime: number;
  path: Array<{ x: number; y: number }>;
  _el: HTMLElement | null;
  _flipEl: HTMLElement | null;
}

const waddlersState = new Map<string, CharacterState>();

// ── Browser synthesized audio chimes (Web Audio API) ─────────────────────────

let audioCtx: AudioContext | null = null;

function getAudioContext(): AudioContext | null {
  if (typeof window === "undefined") return null;
  const AudioCtxClass = window.AudioContext || (window as any).webkitAudioContext;
  if (!AudioCtxClass) return null;
  if (!audioCtx) {
    audioCtx = new AudioCtxClass();
  }
  if (audioCtx.state === "suspended") {
    void audioCtx.resume();
  }
  return audioCtx;
}

function playChime(type: "finished" | "waiting"): void {
  try {
    const ctx = getAudioContext();
    if (!ctx) return;

    const now = ctx.currentTime;
    
    if (type === "finished") {
      // Completed chime: A beautiful upward major chord chime sequence (C4 -> E4 -> G4 -> C5)
      const notes = [261.63, 329.63, 392.00, 523.25];
      notes.forEach((freq, idx) => {
        const osc = ctx.createOscillator();
        const gain = ctx.createGain();
        
        osc.type = "sine";
        osc.frequency.setValueAtTime(freq, now + idx * 0.12);
        
        gain.gain.setValueAtTime(0, now + idx * 0.12);
        gain.gain.linearRampToValueAtTime(0.15, now + idx * 0.12 + 0.04);
        gain.gain.exponentialRampToValueAtTime(0.0001, now + idx * 0.12 + 0.6);
        
        osc.connect(gain);
        gain.connect(ctx.destination);
        
        osc.start(now + idx * 0.12);
        osc.stop(now + idx * 0.12 + 0.6);
      });
    } else if (type === "waiting") {
      // Warning chime: A pleasant soft woodwind-like two-tone chime (A4 -> E4) using a triangle wave
      const notes = [440.00, 329.63];
      notes.forEach((freq, idx) => {
        const osc = ctx.createOscillator();
        const gain = ctx.createGain();
        
        osc.type = "triangle";
        osc.frequency.setValueAtTime(freq, now + idx * 0.25);
        
        gain.gain.setValueAtTime(0, now + idx * 0.25);
        gain.gain.linearRampToValueAtTime(0.12, now + idx * 0.25 + 0.05);
        gain.gain.exponentialRampToValueAtTime(0.0001, now + idx * 0.25 + 0.8);
        
        osc.connect(gain);
        gain.connect(ctx.destination);
        
        osc.start(now + idx * 0.25);
        osc.stop(now + idx * 0.25 + 0.8);
      });
    }
  } catch (err) {
    console.warn("Could not play synthesized audio chime:", err);
  }
}

// Helper to classify sub-action tools based on keywords in last_output
function getAgentSubAction(output: string): "reading" | "writing" | "executing" | "idle" {
  if (!output) return "idle";
  const lower = output.toLowerCase();
  
  if (
    lower.includes("view_file") ||
    lower.includes("grep_search") ||
    lower.includes("list_dir") ||
    lower.includes("search_web") ||
    lower.includes("read_file") ||
    lower.includes("view file") ||
    lower.includes("searching") ||
    lower.includes("reading") ||
    lower.includes("fetching")
  ) {
    return "reading";
  }
  
  if (
    lower.includes("write_to_file") ||
    lower.includes("replace_file_content") ||
    lower.includes("multi_replace_file_content") ||
    lower.includes("editing file") ||
    lower.includes("writing file") ||
    lower.includes("saving") ||
    lower.includes("replacing")
  ) {
    return "writing";
  }
  
  if (
    lower.includes("run_command") ||
    lower.includes("running command") ||
    lower.includes("executing") ||
    lower.includes("compiling") ||
    lower.includes("building") ||
    lower.includes("testing") ||
    lower.includes("npm run") ||
    lower.includes("vitest")
  ) {
    return "executing";
  }
  
  return "idle";
}
let loopRunning = false;
let mapVisible = false;

export function setMapPanelVisible(visible: boolean): void {
  mapVisible = visible;
  if (visible) {
    startAnimationLoop();
  }
}

function startAnimationLoop(): void {
  if (loopRunning) return;
  loopRunning = true;
  let lastTime = performance.now();

  function tick(now: number) {
    if (!mapVisible || document.visibilityState === "hidden") {
      loopRunning = false;
      return;
    }

    const dt = Math.min((now - lastTime) / 1000, DT_CLAMP_SEC);
    lastTime = now;

    const speed = WADDLER_SPEED_PX_PER_SEC * dt;
    let needsRender = false;

    for (const [id, char] of waddlersState.entries()) {
      // Manage speech bubble timers
      if (char.speech && now - char.speechTime > SPEECH_DURATION_MS) {
        char.speech = null;
        needsRender = true;
      }

      if (char.path && char.path.length > 0) {
        char.action = "walking";
        const targetNode = char.path[0];
        const targetPx = targetNode.x * TILE_SIZE;
        const targetPy = targetNode.y * TILE_SIZE;

        const dx = targetPx - char.x;
        const dy = targetPy - char.y;
        const dist = Math.sqrt(dx * dx + dy * dy);

        if (dist < speed) {
          char.x = targetPx;
          char.y = targetPy;
          char.path.shift();

          if (char.path.length === 0) {
            char.action = char.status === "working" ? "working" : "idle";
            const barn = BUILDING_POSITIONS.barn;
            if (barn && char.status === "finished" && char.targetGridX === barn.gridX && char.targetGridY === barn.gridY) {
              char.hidden = true;
            }
            needsRender = true;
          }
        } else {
          char.x += (dx / dist) * speed;
          char.y += (dy / dist) * speed;
        }

        // Cache DOM element handles
        if (!char._el || !char._el.isConnected) {
          char._el = document.getElementById(`waddler-wrapper-${id}`);
          char._flipEl = char._el?.querySelector(".v-goose-flip") || null;
        }

        const el = char._el;
        if (el) {
          el.style.transform = `translate(${char.x + char.offsetX + HALF_TILE}px, ${char.y + char.offsetY + HALF_TILE}px)`;
          const flipEl = char._flipEl;
          if (flipEl && Math.abs(dx) > 0.5) {
            flipEl.style.transform = dx > 0 ? "scaleX(1)" : "scaleX(-1)";
          }
        }
      } else {
        // Character is idle or working on tile
        if (!char._el || !char._el.isConnected) {
          char._el = document.getElementById(`waddler-wrapper-${id}`);
        }
        const el = char._el;
        if (el) {
          el.style.transform = `translate(${char.x + char.offsetX + HALF_TILE}px, ${char.y + char.offsetY + HALF_TILE}px)`;
        }

        // Programmatic micro-wandering patrol around building
        if (Math.random() < 0.005) {
          const bKey = char.status === "working" ? getHomeBuildingKey(char.template) : getHomeBuildingKey(char.template);
          const building = BUILDING_POSITIONS[bKey];
          if (building) {
            // Target a random tile near building
            const dx = Math.floor(Math.random() * 3) - 1;
            const dy = Math.floor(Math.random() * 3) - 1;
            const tx = Math.max(0, Math.min(TOWN_MAP_GRID[0].length - 1, building.gridX + dx));
            const ty = Math.max(0, Math.min(TOWN_MAP_GRID.length - 1, building.gridY + dy));
            const path = findPath(Math.floor(char.x / TILE_SIZE), Math.floor(char.y / TILE_SIZE), tx, ty);
            if (path.length > 0) {
              char.path = path;
              char.targetGridX = tx;
              char.targetGridY = ty;
            }
          }
        }
      }
    }

    if (needsRender) {
      renderWaddlersLayer();
    }

    requestAnimationFrame(tick);
  }

  requestAnimationFrame(tick);
}

// ── Bind Real SSE Active Sessions Stream ──────────────────────────────────────

export async function updateAndRenderMap(): Promise<void> {
  const city = cityScope();
  if (!city || !mapVisible) return;

  initMapGrid();

  const { data, error } = await api.GET("/v0/city/{cityName}/sessions", {
    params: { path: { cityName: city }, query: { state: "active", peek: true } },
  });

  if (error || !data?.items) {
    return;
  }

  const sessions = data.items;

  // Fetch pending status for active sessions in parallel
  const pendingResults = await Promise.all(
    sessions.map(async (session) => {
      const res = await api.GET("/v0/city/{cityName}/session/{id}/pending", {
        params: { path: { cityName: city, id: session.id } },
      });
      return { id: session.id, pending: Boolean(res.data?.pending) };
    })
  ).catch((err) => {
    console.error("Error fetching session pending status:", err);
    return [];
  });
  const pendingMap = new Map<string, boolean>(
    pendingResults.map((r) => [r.id, r.pending])
  );

  const liveIds = new Set(sessions.map((s) => s.id));
  const now = performance.now();

  for (const session of sessions) {
    const template = session.template || "generic";
    const homeBuildingKey = getHomeBuildingKey(template);
    const targetBuilding = BUILDING_POSITIONS[homeBuildingKey] || BUILDING_POSITIONS.market;

    if (!waddlersState.has(session.id)) {
      const rx = targetBuilding.gridX * TILE_SIZE;
      const ry = targetBuilding.gridY * TILE_SIZE;

      waddlersState.set(session.id, {
        id: session.id,
        template,
        x: rx,
        y: ry,
        offsetX: Math.random() * 40 - 20,
        offsetY: Math.random() * 20 - 10,
        targetGridX: targetBuilding.gridX,
        targetGridY: targetBuilding.gridY,
        status: "idle",
        subAction: "idle",
        hasPending: false,
        action: "idle",
        hidden: false,
        speech: null,
        speechTime: 0,
        path: [],
        _el: null,
        _flipEl: null,
      });
    }

    const char = waddlersState.get(session.id)!;
    const isWorking = !!session.active_bead;
    const hasPending = pendingMap.get(session.id) ?? false;
    const subAction = getAgentSubAction(session.last_output);

    // Track transitions for auditory chimes
    const wasPending = char.hasPending;
    const wasWorking = char.status === "working";

    char.hasPending = hasPending;
    char.subAction = subAction;
    char.status = hasPending ? "questions" : isWorking ? "working" : "idle";

    // Play chimes on state transitions
    if (!wasPending && hasPending) {
      playChime("waiting");
    } else if (wasWorking && !isWorking && !hasPending) {
      playChime("finished");
    }

    // Set speech bubbles based on agent's real-time outputs
    if (session.last_output && session.last_output.trim().length > 0) {
      const cleanOutput = session.last_output.replace(/<[^>]*>/g, "").trim();
      if (char.speech !== cleanOutput && cleanOutput.length > 2) {
        let text = cleanOutput;
        if (text.length > SPEECH_MAX_LENGTH) {
          text = text.substring(0, SPEECH_MAX_LENGTH) + "...";
        }
        char.speech = text;
        char.speechTime = now;
      }
    }

    // Dynamic pathfinding trigger on workspace task assignment shifts
    let targetKey = homeBuildingKey;
    if (isWorking) {
      // If working, head to the designated work subsystem building if we can map it
      if (session.active_bead?.startsWith("ga-") || session.active_bead?.includes("-")) {
        // Standard beads are processed inside Dolt Vault (factory)
        targetKey = "factory";
      } else {
        targetKey = homeBuildingKey;
      }
    }

    const targetB = BUILDING_POSITIONS[targetKey] || BUILDING_POSITIONS.market;
    const wantsNewTarget = char.targetGridX !== targetB.gridX || char.targetGridY !== targetB.gridY;

    if (wantsNewTarget) {
      const cx = Math.floor(char.x / TILE_SIZE);
      const cy = Math.floor(char.y / TILE_SIZE);
      const path = findPath(cx, cy, targetB.gridX, targetB.gridY);
      if (path.length > 0) {
        char.targetGridX = targetB.gridX;
        char.targetGridY = targetB.gridY;
        char.path = path;
        char.action = "walking";
        char.hidden = false;
        startAnimationLoop();
      }
    }
  }

  // Remove stale waddlers
  for (const id of waddlersState.keys()) {
    if (!liveIds.has(id)) {
      waddlersState.delete(id);
    }
  }

  renderWaddlersLayer();
}

// ── Draw SVG Elements ────────────────────────────────────────────────────────

function renderWaddlersLayer(): void {
  const layer = byId("waddlers-layer");
  if (!layer) return;

  const htmlBuffer: string[] = [];
  const chars = Array.from(waddlersState.values());

  chars.forEach((c) => {
    const speechBubble = c.speech
      ? `<foreignObject x="-100" y="-60" width="200" height="40" style="overflow: visible;">
           <div class="v-speech"><span class="v-speech-inner">${escapeSVGText(c.speech)}</span></div>
         </foreignObject>`
      : "";

    const hasHat = c.template.toLowerCase().includes("supervisor") || c.template.toLowerCase().includes("mayor")
      ? `<use href="#top-hat"/>`
      : "";

    let accessory = "";
    if (c.hasPending) {
      accessory = `<use href="#acc-question-mark"/>`;
    } else if (c.subAction === "reading") {
      accessory = `<use href="#acc-book"/>`;
    } else if (c.subAction === "writing") {
      accessory = `<use href="#acc-laptop"/>`;
    } else if (c.subAction === "executing") {
      accessory = `<use href="#acc-wrench"/>`;
    }

    htmlBuffer.push(`
      <g id="waddler-wrapper-${c.id}" class="v-goose-wrapper ${c.hidden ? "v-goose-hidden" : ""}" style="transform: translate(${c.x + c.offsetX + HALF_TILE}px, ${c.y + c.offsetY + HALF_TILE}px);">
        ${speechBubble}
        <foreignObject x="-32" y="-64" width="64" height="64" style="overflow: visible;">
          <div class="v-goose-anim ${c.action}" data-session-id="${c.id}" style="width: 64px; height: 64px;">
            <div class="v-goose-flip" style="width: 100%; height: 100%; transition: transform 0.2s;">
              <svg viewBox="0 0 64 64" width="100%" height="100%">
                <use href="#goose"/>
                ${hasHat}
                ${accessory}
              </svg>
            </div>
          </div>
        </foreignObject>
        <foreignObject x="-60" y="-5" width="120" height="30" style="overflow: visible;">
          <div class="v-nameplate" style="text-align: center;">${escapeSVGText(c.template)}</div>
        </foreignObject>
      </g>
    `);
  });

  layer.innerHTML = htmlBuffer.join("\n");
}

function escapeSVGText(str: string): string {
  return str
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&apos;");
}

// ── Main UI Entrypoint ───────────────────────────────────────────────────────

export function renderTownMap(): void {
  const container = byId("village-container");
  if (!container) return;

  initMapGrid();

  const isDay = new Date().getHours() > 6 && new Date().getHours() < 18;

  const bgHtml = generateBackgroundHtml();

  const buildingsHtml = Object.values(BUILDING_POSITIONS)
    .map((b) => `
      <g transform="translate(${b.gridX * TILE_SIZE}, ${b.gridY * TILE_SIZE})">
        <g transform="translate(-80, -150)" class="v-building-art">${b.svg}</g>
        <foreignObject x="-75" y="-30" width="150" height="40" style="overflow: visible;">
          <div class="v-building-label" style="text-align: center;">${b.label}</div>
        </foreignObject>
      </g>
    `)
    .join("\n");

  container.innerHTML = `
    <div class="village-viewport ${isDay ? "day" : ""}">
      <svg class="terrain-layer" role="img" aria-label="Gas City steampunk village map" viewBox="0 0 ${MAP_WIDTH} ${MAP_HEIGHT}" preserveAspectRatio="xMidYMid meet">
        <title>Gas City Village</title>
        ${SvgDefs}
        <rect x="-5000" y="-5000" width="10000" height="10000" fill="url(#grass-pat)"/>
        ${bgHtml}
        ${buildingsHtml}
        <g id="waddlers-layer"></g>
      </svg>
    </div>
  `;

  renderWaddlersLayer();

  // Install delegated click event handler to open logs drawer
  if (container && !(container as any)._clickBound) {
    (container as any)._clickBound = true;
    container.addEventListener("click", (evt) => {
      let curr = evt.target as HTMLElement | SVGElement | null;
      let id: string | null = null;
      while (curr && curr !== container) {
        if (curr.getAttribute && curr.getAttribute("data-session-id")) {
          id = curr.getAttribute("data-session-id");
          break;
        }
        curr = (curr.parentElement || curr.parentNode) as HTMLElement | SVGElement | null;
      }
      if (!id) return;
      const char = waddlersState.get(id);
      if (char) {
        void openLogDrawer(char.id, char.template);
      }
    });
  }
}
