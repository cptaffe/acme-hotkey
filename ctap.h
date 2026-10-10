#pragma once
#include <CoreGraphics/CoreGraphics.h>

void startEventTap(void);
void ackNext(int forward);   /* called from Go: 1=forward original, 0=drop */

/* defined in frontapp.m */
const char *frontmostProcName(void);

void clearFilter(void);
void addFilterProc(const char *name);
void clearGrabs(void);
void addGrab(unsigned short kc, unsigned long long mods);
void setListenerCount(int n);
void drainAllPending(void); /* drop all C-side pending events; call on reader disconnect */

void        initKeyNames(void);           /* build key-name table from current layout */
const char *virtualKeyName(uint16_t kc); /* canonical name for a virtual key code, or NULL */
