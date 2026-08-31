//go:build fixture_source

#include <iostream>

int main() {
    long long a, b;
    if (!(std::cin >> a >> b)) return 3;
    if (a < -1000000000LL || a > 1000000000LL || b < -1000000000LL || b > 1000000000LL) return 3;
    std::cin >> std::ws;
    if (std::cin.peek() != std::char_traits<char>::eof()) return 3;
    return 0;
}
