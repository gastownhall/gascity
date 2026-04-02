# Products — Domain Knowledge

What each product does, who it's for, and key context that agents need when working on them.

### Villa AI Planogram (villa_ai_planogram)
**What:** AI-powered shelf analysis for Villa Market (Thai retail chain). Takes photos of store shelves, detects products using YOLO/SAM2, generates planograms (shelf layout diagrams).
**Users:** Villa Market store managers and merchandising team.
**Key context:** Mobile app for photo capture (Expo/React Native), FastAPI backend for ML inference, Next.js dashboard for planogram review. Dashboard must run in production mode in Docker (no next dev — causes CPU explosion).

### Villa ALC AI (villa_alc_ai)
**What:** AI alcohol concierge for Villa Market. Helps customers find and learn about wines, spirits, and cocktails. WhatsApp gateway for customer interaction.
**Users:** Villa Market alcohol department customers.
**Key context:** FastAPI backend, chat-based interface. WhatsApp integration is the primary channel (P1 beads).

### OfficeWorld (officeworld)
**What:** GBA-style isometric office visualization showing agent activity in real-time. Agents appear as characters moving around an office, working at desks, having meetings.
**Users:** Internal — the user and team for visualizing agent work.
**Key context:** Vite/React frontend, game-like UI. Kanban board integration is a P0.

### Deepwork Site (deepwork_site)
**What:** Company website at deepwork.art. Product showcase, interactive demos, company info.
**Users:** External — potential customers, investors, partners.
**Key context:** Launch prep is highest priority. Needs demos (SAM modal, model playground), product screenshots, Vercel deployment, and domain setup. 4 P0 beads.

### Content Studio (content_studio)
**What:** Content pipeline for processing and organizing "brain dumps" — raw content that gets refined into structured output.
**Users:** Internal content team.
**Key context:** Early stage. Pipeline architecture designed but minimal implementation.

### Media Studio (media_studio)
**What:** AI image generation pipeline using ComfyUI. AI influencer content, product photography, marketing materials.
**Users:** Internal marketing/creative.
**Key context:** ComfyUI runs on GPU 4 (dynamically uses 5-7). Planned but minimal activity.
