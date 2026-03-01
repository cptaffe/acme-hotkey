#pragma once
#include <ApplicationServices/ApplicationServices.h>
#include <Carbon/Carbon.h>
#include <CoreGraphics/CoreGraphics.h>

void startEventTap(void);
void reInjectKey(unsigned short kc, unsigned long long flags);

void clearFilter(void);
void addFilterProc(const char *name);
