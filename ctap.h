#pragma once
#include <CoreGraphics/CoreGraphics.h>

void startEventTap(void);
void reInjectKey(unsigned short kc, unsigned long long flags);

/* defined in frontapp.m */
const char *frontmostProcName(void);

void clearFilter(void);
void addFilterProc(const char *name);
