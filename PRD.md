# Coves Product Requirements Document (PRD)
## Digital Community Infrastructure Platform

---

## 1. Executive Summary

**Vision**: A federated forum platform that ensures a solid framework for digital community building and meaningful information discussion.

**Core Values**:
- User sovereignty through data portability
- Community autonomy and self-governance
- Cross-platform interoperability

---

## 2. MVP Scope Definition

### Phase 1: Core Forum Platform (Web)

#### Must Have:
- **Indigo PDS Integration** - Use existing atProto infrastructure (no CAR file reimplementation!)
- User registration with phone verification (verified badge)
- Community creation, subscription, and discovery
- Post creation (text initially, then image/video/article)
- Threaded comments with upvoting
- Three feed types:
  - **Home**: Personalized based on subscriptions with 1-5 visibility slider per community
  - **All**: Global content discovery
  - **Community**: Standard forum sorting (hot/new/top)
- Read state tracking (hide read posts, history tab)
- Content tagging system (helpful/spam/hostile)
- User blocking capabilities

#### Defer to Post-MVP:
- Community wikis
- Advanced reputation system
- Sortition/tribunal moderation
- Edit history tracking
- Plugin/bot system

### Phase 2: Federation (MVP+)
- Bluesky post display (read-only via Indigo firehose)
- ActivityPub bidirectional support
- Federation source indicators

### Phase 3: Mobile Apps (Separate Repo)
- iOS/Android native apps
- TikTok-style horizontal scrolling feed
- Offline-first architecture
- Push notifications

---

## 3. Technical Architecture

### Backend Stack:
- **PDS Layer**: Indigo PDS implementation (handles atProto, CAR files, firehose)
- **Application Layer**: Go services for Coves-specific features
- **Databases**:
  - Indigo PDS's PostgreSQL (user repos, DID management)
  - Separate PostgreSQL for AppView (queries, read states, community data)
- **APIs**: XRPC protocol (leverage Indigo's implementation)
- **Identity**: atProto DIDs with phone verification

### Frontend Stack:
- **Web**: React/Next.js
- **Mobile**: React Native or Flutter (separate repository)
- **State Management**: Zustand/Redux with offline sync

---

## 4. Development Roadmap

### Week 1-2: Foundation
- Deploy Indigo PDS instance
- Set up AppView database
- Create core Lexicon schemas (community, post, comment)
- Implement phone verification flow

### Week 3-4: User & Identity
- User registration/login via Indigo PDS
- Profile management
- DID resolution and handle system
- Basic user settings

### Week 5-6: Communities
- Community CRUD operations
- Subscription management
- Community discovery page
- Basic moderation tools

### Week 7-8: Content Creation
- Post creation (text, then multimedia)
- Comment threads
- Upvoting system
- Content tagging

### Week 9-10: Feed System
- Home feed with subscription slider
- Community feeds with sorting
- All/global feed
- Read state tracking and history

### Week 11-12: Polish & Testing
- Search functionality
- User blocking
- Performance optimization
- Security audit
- Alpha deployment

### Post-MVP: Federation
- Subscribe to Indigo firehose for Bluesky content
- ActivityPub adapter service
- Cross-platform user mapping

---

## 5. Mobile Application Strategy

**Approach**: Separate repository, API-first design

### MVP Features:
- Horizontal swipe navigation between posts
- Offline caching with background sync
- Push notifications for interactions
- Native performance optimization

**Timeline**: Begin after web MVP validation (Month 4-6)

---

## 6. Key Technical Decisions

### Leveraging Indigo:
- Use Indigo PDS for all atProto infrastructure
- Subscribe to Indigo firehose instead of building our own
- Focus engineering on Coves-specific features

### Data Architecture:
- User content → Indigo PDS (portable)
- Read states/analytics → AppView (non-portable, privacy-focused)
- No ads, tracking, or user data monetization

### Open Source:
- MIT license
- Public repository
- Community contributions welcome

---

## 7. Success Metrics

### MVP Launch (3 months):
- 50+ active communities
- 500+ registered users
- <2 second page loads
- 99.5% uptime
- Successful Bluesky content display

### 6 Month Goals:
- 500+ communities
- 5,000+ MAU
- Mobile app launched
- Federation with 3+ platforms

---

## 8. Resource Requirements

### Team:
- 2 Backend Engineers (Go, atProto experience)
- 1 Frontend Engineer (React)
- 1 DevOps (part-time, Indigo deployment)
- 1 Mobile Developer (post-MVP)

### Infrastructure:
- Indigo PDS instance: ~$200/month
- AppView database: ~$100/month
- CDN/Storage: ~$100/month
- **Total**: ~$400-500/month initially

---

## 9. Risk Mitigation

| Risk | Mitigation |
|------|------------|
| Indigo PDS complexity | Start with vanilla deployment, customize gradually |
| Moderation at scale | Launch with simple tagging, evolve based on community needs |
| Federation conflicts | Phased rollout, Bluesky read-only first |
| Mobile development | Consider PWA if native timeline slips |
| Community adoption | Focus on unique features (read states, visibility slider) |

---

## 10. Immediate Next Steps

### Week 1:
- Deploy Indigo PDS test instance
- Design Coves-specific Lexicon schemas
- Set up development environment

### Week 2:
- Implement phone verification service
- Create first XRPC endpoints for communities
- Begin frontend scaffolding

### Week 3:
- User registration flow
- Community creation
- Basic post functionality

---

## 11. Long-term Vision (Post-MVP)

- Sortition-based moderation experiments
- Community wikis and documentation
- Plugin system for community bots
- Advanced reputation mechanics
- Full ActivityPub federation
- Possible Mastodon integration
- Community forking via DIDs
- Incognito browsing mode

---

## 12. Differentiators

**Why Coves vs Reddit/Lemmy/Discourse?**
- True data portability via atProto
- Subscription visibility slider (unique feed control)
- Read state tracking across devices
- Phone verification for trust
- Federation-first, not an afterthought
- Community-driven moderation models
- No corporate ownership or ads

---

## Feature Details from Domain Knowledge

### Feed System Specifications

#### Home Feed:
- **UI**: TikTok-style scrollable feed (horizontal swipe)
- **Personalization**:
  - Subscription slider (1-5 scale per community)
    - 1 = Only best/most popular content
    - 5 = Show all content
  - Read state tracking with history tab
  - Read history stored in AppView (not user repos)

#### Community Feed:
- Standard sorting: hot, top (day/month/year), new
- Respects read state
- Community-specific rules application

### Community Features

#### Core Capabilities:
- Creation and blocking
- Wiki maintenance
- NSFW toggling
- Subscription management

#### Reputation System:
- Gained through posts, comments, positive tags
- Affects member access levels
- Influences comment ordering
- Voting weight based on reputation

#### Rules System:
- Democratic rule voting
- Post type restrictions (e.g., text-only)
- Website blocklists
- Geolocation restrictions

#### Moderation Models:
1. **Sortition-Based**:
   - Tag-based removal with tribunal review
   - Minimum reputation for tribunal service

2. **Traditional**:
   - Moderator hierarchy
   - Community override capability
   - Hybrid approach with user feedback

### Post System

#### Types:
- Text (MVP)
- Video
- Image
- Article
- Microblog (for Bluesky posts)

#### Features:
- Upvoting
- Tagging (helpful/spam/hostile)
- Share tracking
- Comment ownership
- Federation source indicators

### User System

#### Identity:
- Phone verification (grants verified status)
- atProto DID system
- Platform federation tracking

#### Username System:
- Random generation patterns:
  - "AdjectiveNoun" (e.g., "BraveEagle")
  - "AdjectiveAdjectiveNoun" (e.g., "SmallQuietMouse")

#### Features:
- User blocking
- Notification muting
- Post saving
- View history

### Federation Approach

#### atProto (Bluesky):
- Display posts inline as references
- Posts-only initially (comments later)

#### ActivityPub:
- Two-way compatibility
- Instance → Hub mapping
- Community mapping
- User DID assignment
- Action translation to Coves lexicon

---

*Last Updated: January 2025*
*Version: 1.0*

This PRD focuses on shipping a working MVP in 3 months by leveraging existing Indigo infrastructure, then iterating based on real community feedback.