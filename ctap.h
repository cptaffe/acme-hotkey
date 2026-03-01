#pragma once
#include <CoreGraphics/CoreGraphics.h>

void startEventTap(void);
void ackNext(int forward);   /* called from Go: 1=forward original, 0=drop */

/* defined in frontapp.m */
const char *frontmostProcName(void);

void clearFilter(void);
void addFilterProc(const char *name);
void setListenerCount(int n);
